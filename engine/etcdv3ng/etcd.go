// package etcdng contains the implementation of the Etcd-backed engine, where all vulcand properties are implemented as directories or keys.
// this engine is capable of watching the changes and generating events.
package etcdv3ng

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"errors"
	log "github.com/Sirupsen/logrus"
	etcd "github.com/coreos/etcd/clientv3"
	"github.com/vulcand/vulcand/engine"
	"github.com/vulcand/vulcand/plugin"
	"github.com/vulcand/vulcand/secret"
	"golang.org/x/net/context"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
)

type ng struct {
	nodes         []string
	registry      *plugin.Registry
	etcdKey       string
	client        *etcd.Client
	context       context.Context
	cancelFunc    context.CancelFunc
	logsev        log.Level
	options       Options
	requireQuorum bool
}

type Options struct {
	EtcdConsistency         string
	EtcdCaFile              string
	EtcdCertFile            string
	EtcdKeyFile             string
	EtcdSyncIntervalSeconds int64
	Box                     *secret.Box
}

func New(nodes []string, etcdKey string, registry *plugin.Registry, options Options) (engine.Engine, error) {
	n := &ng{
		nodes:    nodes,
		registry: registry,
		etcdKey:  etcdKey,
		options:  options,
	}
	if err := n.reconnect(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *ng) Close() {
	if n.cancelFunc != nil {
		n.cancelFunc()
	}
}

func (n *ng) GetSnapshot() (*engine.Snapshot, error) {
	response, err := n.client.Get(n.context, n.etcdKey, etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		return nil, err
	}
	s := &engine.Snapshot{Index: uint64(response.Header.Revision)}
	for _, keyValue := range response.Kvs {
		switch suffix(string(keyValue.Key)) {
		case "frontends":
			s.FrontendSpecs, err = n.parseFrontends(filterBySuffix(response.Kvs, string(keyValue.Key)))
			if err != nil {
				return nil, err
			}
		case "backends":
			s.BackendSpecs, err = n.parseBackends(filterBySuffix(response.Kvs, string(keyValue.Key)))
			if err != nil {
				return nil, err
			}
		case "hosts":
			s.Hosts, err = n.parseHosts(filterBySuffix(response.Kvs, string(keyValue.Key)))
			if err != nil {
				return nil, err
			}
		case "listeners":
			s.Listeners, err = n.parseListeners(filterBySuffix(response.Kvs, string(keyValue.Key)))
			if err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (n *ng) parseFrontends(keyValues []*mvccpb.KeyValue, skipMiddlewares ...bool) ([]engine.FrontendSpec, error) {
	frontendSpecs := []engine.FrontendSpec{}

	for _, keyValue := range keyValues {
		if frontendId := suffix(string(keyValue.Key)); suffix(prefix(string(keyValue.Key))) == "frontend" {
			frontend, err := engine.FrontendFromJSON(n.registry.GetRouter(), []byte(keyValue.Value), frontendId)
			if err != nil {
				return nil, err
			}

			if len(skipMiddlewares) == 1 && skipMiddlewares[0] {
				break
			}

			//get all keys under this frontend
			subKeyValues := filterBySuffix(keyValues, string(keyValue.Key)) //Get all keys below this frontend "/vulcand/frontends/foo/*"

			middlewares := []engine.Middleware{}
			for _, subKeyValue := range subKeyValues {
				if middlewareId := suffix(string(subKeyValue.Key)); suffix(prefix(string(subKeyValue.Key))) == "middlewares" {
						middleware, err := engine.MiddlewareFromJSON([]byte(subKeyValue.Value), n.registry.GetSpec, middlewareId)
						if err != nil {
							return nil, err
						}
						middlewares = append(middlewares, *middleware)
				}
			}


			frontendSpec := engine.FrontendSpec{
				Frontend: *frontend,
				Middlewares: middlewares,
			}

			frontendSpecs = append(frontendSpecs, frontendSpec)
		}
	}
	return frontendSpecs, nil
}

func (n *ng) parseBackends(keyValues []*mvccpb.KeyValue, skipServers ...bool) ([]engine.BackendSpec, error) {
	backendSpecs := []engine.BackendSpec{}

	for _, keyValue := range keyValues {
		if backendId := suffix(string(keyValue.Key)); suffix(prefix(string(keyValue.Key))) == "backend" {
			backend, err := engine.BackendFromJSON([]byte(keyValue.Value), backendId)
			if err != nil {
				return nil, err
			}

			if len(skipServers) == 1 && skipServers[0] {
				break
			}

			//get all keys under this frontend
			subKeyValues := filterBySuffix(keyValues, string(keyValue.Key)) //Get all keys below this frontend "/vulcand/frontends/foo/*"

			servers := []engine.Server{}

			for _, subKeyValue := range subKeyValues {
				if serverId := suffix(string(subKeyValue.Key)); suffix(prefix(string(subKeyValue.Key))) == "servers" {
					server, err := engine.ServerFromJSON([]byte(subKeyValue.Value), serverId)
					if err != nil {
						return nil, err
					}
					servers = append(servers, *server)
				}
			}

			backendSpec := engine.BackendSpec{
				Backend: *backend,
				Servers: servers,
			}

			backendSpecs = append(backendSpecs, backendSpec)

		}
	}
	return backendSpecs, nil
}

func (n *ng) parseHosts(keyValues []*mvccpb.KeyValue) ([]engine.Host, error) {
	hosts := []engine.Host{}
	for _, keyValue := range keyValues {
		if hostname := suffix(string(keyValue.Key)); suffix(prefix(string(keyValue.Key))) == "host" {
			var sealedHost host
			if err := json.Unmarshal([]byte(keyValue.Value), &sealedHost); err != nil {
				return nil, err
			}
			var keyPair *engine.KeyPair
			if len(sealedHost.Settings.KeyPair) != 0 {
				if err := n.openSealedJSONVal(sealedHost.Settings.KeyPair, &keyPair); err != nil {
					return nil, err
				}
			}
			host, err := engine.NewHost(hostname, engine.HostSettings{Default: sealedHost.Settings.Default, KeyPair: keyPair, OCSP: sealedHost.Settings.OCSP})
			if err != nil {
				return nil, err
			}
			if host.Name != hostname {
				return nil, fmt.Errorf("Host %s parameters missing", hostname)
			}
			hosts = append(hosts, *host)
		}
	}
	return hosts, nil
}

func (n *ng) parseListeners(keyValues []*mvccpb.KeyValue) ([]engine.Listener, error) {
	listeners := []engine.Listener{}
	for _, keyValue := range keyValues {
		if listenerId := suffix(string(keyValue.Key)); suffix(prefix(string(keyValue.Key))) == "listeners" {
			listener, err := engine.ListenerFromJSON([]byte(keyValue.Value), listenerId)
			if err != nil {
				return nil, err
			}
			listeners = append(listeners, *listener)
		}
	}
	return listeners, nil
}

func (n *ng) GetLogSeverity() log.Level {
	return n.logsev
}

func (n *ng) SetLogSeverity(sev log.Level) {
	n.logsev = sev
	log.SetLevel(n.logsev)
}

func (n *ng) reconnect() error {
	n.Close()
	var client *etcd.Client
	cfg := n.getEtcdClientConfig()
	var err error
	if client, err = etcd.New(cfg); err != nil {
		return err
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	n.context = ctx
	n.cancelFunc = cancelFunc
	n.client = client
	n.requireQuorum = true
	if n.options.EtcdConsistency == "WEAK" {
		n.requireQuorum = false
	}
	return nil
}

func (n *ng) getEtcdClientConfig() etcd.Config {
	return etcd.Config{
		Endpoints: n.nodes,
	}
}

func (n *ng) GetRegistry() *plugin.Registry {
	return n.registry
}

func (n *ng) GetHosts() ([]engine.Host, error) {
	hosts := []engine.Host{}
	vals, err := n.getKeysByImmediatePrefix(n.etcdKey, "hosts")
	if err != nil {
		return nil, err
	}
	for _, hostKey := range vals {
		host, err := n.GetHost(engine.HostKey{Name: suffix(hostKey)})
		if err != nil {
			log.Warningf("Invalid host config for %v: %v\n", hostKey, err)
			continue
		}
		hosts = append(hosts, *host)
	}
	return hosts, nil
}

func (n *ng) GetHost(key engine.HostKey) (*engine.Host, error) {
	hostKey := n.path("hosts", key.Name, "host")

	var host *host
	err := n.getJSONVal(hostKey, &host)
	if err != nil {
		return nil, err
	}

	var keyPair *engine.KeyPair
	if len(host.Settings.KeyPair) != 0 {
		if err := n.openSealedJSONVal(host.Settings.KeyPair, &keyPair); err != nil {
			return nil, err
		}
	}

	return engine.NewHost(key.Name, engine.HostSettings{Default: host.Settings.Default, KeyPair: keyPair, OCSP: host.Settings.OCSP})
}

func (n *ng) UpsertHost(h engine.Host) error {
	if h.Name == "" {
		return &engine.InvalidFormatError{Message: "hostname can not be empty"}
	}
	hostKey := n.path("hosts", h.Name, "host")

	val := host{
		Name: h.Name,
		Settings: hostSettings{
			Default: h.Settings.Default,
			OCSP:    h.Settings.OCSP,
		},
	}

	if h.Settings.KeyPair != nil {
		bytes, err := n.sealJSONVal(h.Settings.KeyPair)
		if err != nil {
			return err
		}
		val.Settings.KeyPair = bytes
	}

	return n.setJSONVal(hostKey, val, noTTL)
}

func (n *ng) DeleteHost(key engine.HostKey) error {
	if key.Name == "" {
		return &engine.InvalidFormatError{Message: "hostname can not be empty"}
	}
	return n.deleteKey(n.path("hosts", key.Name))
}

func (n *ng) GetListeners() ([]engine.Listener, error) {
	ls := []engine.Listener{}
	vals, err := n.getVals(n.etcdKey, "listeners")
	if err != nil {
		return nil, err
	}
	for _, p := range vals {
		l, err := n.GetListener(engine.ListenerKey{Id: suffix(p.Key)})
		if err != nil {
			log.Warningf("Invalid listener config for %v: %v\n", n.etcdKey, err)
			continue
		}
		ls = append(ls, *l)
	}
	return ls, nil
}

func (n *ng) GetListener(key engine.ListenerKey) (*engine.Listener, error) {
	bytes, err := n.getVal(n.path("listeners", key.Id))
	if err != nil {
		return nil, err
	}
	l, err := engine.ListenerFromJSON([]byte(bytes), key.Id)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (n *ng) UpsertListener(listener engine.Listener) error {
	if listener.Id == "" {
		return &engine.InvalidFormatError{Message: "listener id can not be empty"}
	}
	return n.setJSONVal(n.path("listeners", listener.Id), listener, noTTL)
}

func (n *ng) DeleteListener(key engine.ListenerKey) error {
	if key.Id == "" {
		return &engine.InvalidFormatError{Message: "listener id can not be empty"}
	}
	return n.deleteKey(n.path("listeners", key.Id))
}

func (n *ng) UpsertFrontend(f engine.Frontend, ttl time.Duration) error {
	if f.Id == "" {
		return &engine.InvalidFormatError{Message: "frontend id can not be empty"}
	}
	if _, err := n.GetBackend(engine.BackendKey{Id: f.BackendId}); err != nil {
		return err
	}
	if err := n.setJSONVal(n.path("frontends", f.Id, "frontend"), f, noTTL); err != nil {
		return err
	}
	if ttl == 0 {
		return nil
	}

	lgr, err := n.client.Grant(n.context, int64(ttl.Seconds()))
	if err != nil {
		return err
	}

	_, err = n.client.Put(n.context, n.path("frontends", f.Id), "", etcd.WithLease(lgr.ID))
	return convertErr(err)
}

func (n *ng) GetFrontends() ([]engine.Frontend, error) {
	key := fmt.Sprintf("%s/frontends", n.etcdKey)
	response, err := n.client.Get(n.context, key, etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		return nil, err
	}
	frontendSpecs, err := n.parseFrontends(response.Kvs, true)
	if err != nil {
		return nil, err
	}
	frontends := make([]engine.Frontend, len(frontendSpecs))
	for i, frontendSpec := range frontendSpecs {
		frontends[i] = frontendSpec.Frontend
	}
	return frontends, nil
}

func (n *ng) GetFrontend(key engine.FrontendKey) (*engine.Frontend, error) {
	frontendKey := n.path("frontends", key.Id, "frontend")

	bytes, err := n.getVal(frontendKey)
	if err != nil {
		return nil, err
	}
	return engine.FrontendFromJSON(n.registry.GetRouter(), []byte(bytes), key.Id)
}

func (n *ng) DeleteFrontend(fk engine.FrontendKey) error {
	if fk.Id == "" {
		return &engine.InvalidFormatError{Message: "frontend id can not be empty"}
	}
	return n.deleteKey(n.path("frontends", fk.Id))
}

func (n *ng) GetBackends() ([]engine.Backend, error) {
	response, err := n.client.Get(n.context, fmt.Sprintf("%s/backends", n.etcdKey), etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		return nil, err
	}
	backendSpecs, err := n.parseBackends(response.Kvs, true)
	if err != nil {
		return nil, err
	}
	backends := make([]engine.Backend, len(backendSpecs))
	for i, backendSpec := range backendSpecs {
		backends[i] = backendSpec.Backend
	}
	return backends, nil
}

func (n *ng) GetBackend(key engine.BackendKey) (*engine.Backend, error) {
	backendKey := n.path("backends", key.Id, "backend")

	bytes, err := n.getVal(backendKey)
	if err != nil {
		return nil, err
	}
	return engine.BackendFromJSON([]byte(bytes), key.Id)
}

func (n *ng) UpsertBackend(b engine.Backend) error {
	if b.Id == "" {
		return &engine.InvalidFormatError{Message: "backend id can not be empty"}
	}
	return n.setJSONVal(n.path("backends", b.Id, "backend"), b, noTTL)
}

func (n *ng) DeleteBackend(bk engine.BackendKey) error {
	if bk.Id == "" {
		return &engine.InvalidFormatError{Message: "backend id can not be empty"}
	}
	fs, err := n.backendUsedBy(bk)
	if err != nil {
		return err
	}
	if len(fs) != 0 {
		return fmt.Errorf("can not delete backend '%v', it is in use by %s", bk, fs)
	}
	_, err = n.client.Delete(n.context, n.path("backends", bk.Id), etcd.WithPrefix())
	return convertErr(err)
}

func (n *ng) GetMiddlewares(fk engine.FrontendKey) ([]engine.Middleware, error) {
	ms := []engine.Middleware{}
	keys, err := n.getVals(n.etcdKey, "frontends", fk.Id, "middlewares")
	if err != nil {
		return nil, err
	}
	for _, p := range keys {
		m, err := n.GetMiddleware(engine.MiddlewareKey{Id: suffix(p.Key), FrontendKey: fk})
		if err != nil {
			log.Warningf("Invalid middleware config for %v (frontend: %v): %v\n", p.Key, fk, err)
			continue
		}
		ms = append(ms, *m)
	}
	return ms, nil
}

func (n *ng) GetMiddleware(key engine.MiddlewareKey) (*engine.Middleware, error) {
	mKey := n.path("frontends", key.FrontendKey.Id, "middlewares", key.Id)
	bytes, err := n.getVal(mKey)
	if err != nil {
		return nil, err
	}
	return engine.MiddlewareFromJSON([]byte(bytes), n.registry.GetSpec, key.Id)
}

func (n *ng) UpsertMiddleware(fk engine.FrontendKey, m engine.Middleware, ttl time.Duration) error {
	if fk.Id == "" || m.Id == "" {
		return &engine.InvalidFormatError{Message: "frontend id and middleware id can not be empty"}
	}
	if _, err := n.GetFrontend(fk); err != nil {
		return err
	}
	return n.setJSONVal(n.path("frontends", fk.Id, "middlewares", m.Id), m, ttl)
}

func (n *ng) DeleteMiddleware(mk engine.MiddlewareKey) error {
	if mk.FrontendKey.Id == "" || mk.Id == "" {
		return &engine.InvalidFormatError{Message: "frontend id and middleware id can not be empty"}
	}
	return n.deleteKey(n.path("frontends", mk.FrontendKey.Id, "middlewares", mk.Id))
}

func (n *ng) UpsertServer(bk engine.BackendKey, s engine.Server, ttl time.Duration) error {
	if s.Id == "" || bk.Id == "" {
		return &engine.InvalidFormatError{Message: "backend id and server id can not be empty"}
	}
	if _, err := n.GetBackend(bk); err != nil {
		return err
	}
	return n.setJSONVal(n.path("backends", bk.Id, "servers", s.Id), s, ttl)
}

func (n *ng) GetServers(bk engine.BackendKey) ([]engine.Server, error) {
	svs := []engine.Server{}
	keys, err := n.getVals(n.etcdKey, "backends", bk.Id, "servers")
	if err != nil {
		return nil, err
	}
	for _, p := range keys {
		srv, err := n.GetServer(engine.ServerKey{Id: suffix(p.Key), BackendKey: bk})
		if err != nil {
			log.Warningf("Invalid server config for %v (backend: %v): %v\n", p.Key, bk, err)
			continue
		}
		svs = append(svs, *srv)
	}
	return svs, nil
}

func (n *ng) GetServer(sk engine.ServerKey) (*engine.Server, error) {
	bytes, err := n.getVal(n.path("backends", sk.BackendKey.Id, "servers", sk.Id))
	if err != nil {
		return nil, err
	}
	return engine.ServerFromJSON([]byte(bytes), sk.Id)
}

func (n *ng) DeleteServer(sk engine.ServerKey) error {
	if sk.Id == "" || sk.BackendKey.Id == "" {
		return &engine.InvalidFormatError{Message: "backend id and server id can not be empty"}
	}
	return n.deleteKey(n.path("backends", sk.BackendKey.Id, "servers", sk.Id))
}

func (n *ng) openSealedJSONVal(bytes []byte, val interface{}) error {
	if n.options.Box == nil {
		return errors.New("need secretbox to open sealed data")
	}
	sv, err := secret.SealedValueFromJSON([]byte(bytes))
	if err != nil {
		return err
	}
	unsealed, err := n.options.Box.Open(sv)
	if err != nil {
		return err
	}
	return json.Unmarshal(unsealed, val)
}

func (n *ng) sealJSONVal(val interface{}) ([]byte, error) {
	if n.options.Box == nil {
		return nil, errors.New("this backend does not support encryption")
	}
	bytes, err := json.Marshal(val)
	if err != nil {
		return nil, err
	}
	v, err := n.options.Box.Seal(bytes)
	if err != nil {
		return nil, err
	}
	return secret.SealedValueToJSON(v)
}

func (n *ng) backendUsedBy(bk engine.BackendKey) ([]engine.Frontend, error) {
	fs, err := n.GetFrontends()
	usedFs := []engine.Frontend{}
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if f.BackendId == bk.Id {
			usedFs = append(usedFs, f)
		}
	}
	return usedFs, nil
}

// Subscribe watches etcd changes and generates structured events telling vulcand to add or delete frontends, hosts etc.
// It is a blocking function.
func (n *ng) Subscribe(changes chan interface{}, afterIdx uint64, cancelC chan bool) error {
	watcher := etcd.NewWatcher(n.client)
	defer watcher.Close()

	watchChan := watcher.Watch(n.context, n.etcdKey, etcd.WithRev(int64(afterIdx)), etcd.WithPrefix())

	for response := range watchChan {
		if response.Canceled {
			log.Infof("Stop watching: graceful shutdown")
			return nil
		}
		if err := response.Err(); err != nil {
			log.Errorf("Stop watching: error: %v", err)
			return err
		}

		for _, event := range response.Events {
			log.Infof("%s", eventToString(event))
			change, err := n.parseChange(event)
			if err != nil {
				log.Warningf("Ignore '%s', error: %s", eventToString(event), err)
				continue
			}
			if change != nil {
				log.Infof("%v", change)
				select {
				case changes <- change:
				case <-cancelC:
					return nil
				}
			}
		}
	}

	return nil
}

type MatcherFn func(*etcd.Event) (interface{}, error)

// Dispatches etcd key changes changes to the etcd to the matching functions
func (n *ng) parseChange(e *etcd.Event) (interface{}, error) {
	// Order parsers from the most to the least frequently used.
	matchers := []MatcherFn{
		n.parseBackendServerChange,
		n.parseBackendChange,
		n.parseFrontendMiddlewareChange,
		n.parseFrontendChange,
		n.parseHostChange,
		n.parseListenerChange,
	}
	for _, matcher := range matchers {
		a, err := matcher(e)
		if a != nil || err != nil {
			return a, err
		}
	}
	return nil, nil
}

func (n *ng) parseHostChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/hosts/([^/]+)(?:/host)?$").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 2 {
		return nil, nil
	}

	hostname := out[1]

	switch e.Type {
	case etcd.EventTypePut:
		host, err := n.GetHost(engine.HostKey{Name: hostname})
		if err != nil {
			return nil, err
		}
		return &engine.HostUpserted{
			Host: *host,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.HostDeleted{
			HostKey: engine.HostKey{Name: hostname},
		}, nil
	}
	return nil, fmt.Errorf("unsupported action for host: %s", e.Type)
}

func (n *ng) parseListenerChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/listeners/([^/]+)").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 2 {
		return nil, nil
	}

	key := engine.ListenerKey{Id: out[1]}

	switch e.Type {
	case etcd.EventTypePut:
		l, err := n.GetListener(key)
		if err != nil {
			return nil, err
		}
		return &engine.ListenerUpserted{
			Listener: *l,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.ListenerDeleted{
			ListenerKey: key,
		}, nil
	}
	return nil, fmt.Errorf("unsupported action on the listener: %s", e.Type)
}

func (n *ng) parseFrontendChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/frontends/([^/]+)(?:/frontend)?$").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 2 {
		return nil, nil
	}
	key := engine.FrontendKey{Id: out[1]}
	switch e.Type {
	case etcd.EventTypePut:
		f, err := n.GetFrontend(key)
		if err != nil {
			return nil, err
		}
		return &engine.FrontendUpserted{
			Frontend: *f,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.FrontendDeleted{
			FrontendKey: key,
		}, nil
	}
	return nil, fmt.Errorf("unsupported action on the frontend: %v %v", e.Kv.Key, e.Type)
}

func (n *ng) parseFrontendMiddlewareChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/frontends/([^/]+)/middlewares/([^/]+)$").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 3 {
		return nil, nil
	}

	fk := engine.FrontendKey{Id: out[1]}
	mk := engine.MiddlewareKey{FrontendKey: fk, Id: out[2]}

	switch e.Type {
	case etcd.EventTypePut:
		m, err := n.GetMiddleware(mk)
		if err != nil {
			return nil, err
		}
		return &engine.MiddlewareUpserted{
			FrontendKey: fk,
			Middleware:  *m,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.MiddlewareDeleted{
			MiddlewareKey: mk,
		}, nil
	}
	return nil, fmt.Errorf("unsupported action on the rate: %s", e.Type)
}

func (n *ng) parseBackendChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/backends/([^/]+)(?:/backend)?$").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 2 {
		return nil, nil
	}
	bk := engine.BackendKey{Id: out[1]}
	switch e.Type {
	case etcd.EventTypePut:
		b, err := n.GetBackend(bk)
		if err != nil {
			return nil, err
		}
		return &engine.BackendUpserted{
			Backend: *b,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.BackendDeleted{
			BackendKey: bk,
		}, nil
	}
	return nil, fmt.Errorf("unsupported node action: %s", e.Type)
}

func (n *ng) parseBackendServerChange(e *etcd.Event) (interface{}, error) {
	out := regexp.MustCompile("/backends/([^/]+)/servers/([^/]+)$").FindStringSubmatch(string(e.Kv.Key))
	if len(out) != 3 {
		return nil, nil
	}

	sk := engine.ServerKey{BackendKey: engine.BackendKey{Id: out[1]}, Id: out[2]}

	switch e.Type {
	case etcd.EventTypePut:
		srv, err := n.GetServer(sk)
		if err != nil {
			return nil, err
		}
		return &engine.ServerUpserted{
			BackendKey: sk.BackendKey,
			Server:     *srv,
		}, nil
	case etcd.EventTypeDelete:
		return &engine.ServerDeleted{
			ServerKey: sk,
		}, nil
	}
	return nil, fmt.Errorf("unsupported action on the server: %s", e.Type)
}

func (n ng) path(keys ...string) string {
	return strings.Join(append([]string{n.etcdKey}, keys...), "/")
}

func (n *ng) setJSONVal(key string, v interface{}, ttl time.Duration) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return n.setVal(key, bytes, ttl)
}

func (n *ng) setVal(key string, val []byte, ttl time.Duration) error {
	glr, err := n.client.Grant(n.context, int64(ttl.Seconds()))
	if err != nil {
		return err
	}

	_, err = n.client.Put(n.context, key, string(val), etcd.WithLease(glr.ID))
	return convertErr(err)
}

func (n *ng) getJSONVal(key string, in interface{}) error {
	val, err := n.getVal(key)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(val), in)
}

func (n *ng) getVal(key string) (string, error) {
	response, err := n.client.Get(n.context, key)
	if err != nil {
		return "", convertErr(err)
	}

	if len(response.Kvs) != 1 {
		return "", &engine.NotFoundError{Message: "Key not found"}
	}

	return string(response.Kvs[0].Value), nil
}

func (n *ng) getKeysByImmediatePrefix(keys ...string) ([]string, error) {
	var out []string
	targetPrefix := strings.Join(keys, "/")
	response, err := n.client.Get(n.context, targetPrefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		if notFound(err) {
			return out, nil
		}
		return nil, err
	}


	//If /this/is/prefix then
	// allow /this/is/prefix/one
	// disallow /this/is/prefix/one/two
	for _, keyValue := range response.Kvs {
		if prefix(string(keyValue.Key)) == targetPrefix {
			out = append(out, string(keyValue.Key))
		}
	}
	return out, nil
}

func (n *ng) getVals(keys ...string) ([]Pair, error) {
	var out []Pair
	response, err := n.client.Get(n.context, strings.Join(keys, "/"), etcd.WithPrefix(), etcd.WithSort(etcd.SortByKey, etcd.SortAscend))
	if err != nil {
		if notFound(err) {
			return out, nil
		}
		return nil, err
	}


	for _, keyValue := range response.Kvs {
		out = append(out, Pair{string(keyValue.Key), string(keyValue.Value)})
	}
	return out, nil
}

func (n *ng) checkKeyExists(key string) error {
	_, err := n.client.Get(n.context, key)
	return convertErr(err)
}

func (n *ng) deleteKey(key string) error {
	_, err := n.client.Delete(n.context, key, etcd.WithPrefix())
	return convertErr(err)
}

type Pair struct {
	Key string
	Val string
}

func suffix(key string) string {
	lastSlashIdx := strings.LastIndex(key, "/")
	if lastSlashIdx == -1 {
		return key
	}
	return key[lastSlashIdx+1:]
}

func prefix(key string) string {
	lastSlashIdx := strings.LastIndex(key, "/")
	if lastSlashIdx == -1 {
		return key
	}
	return key[0:lastSlashIdx]
}

func notFound(e error) bool {
	return e == rpctypes.ErrEmptyKey
}

func convertErr(e error) error {
	if e == nil {
		return nil
	}
	switch e {
	case rpctypes.ErrEmptyKey:
		return &engine.NotFoundError{Message: e.Error()}

	case rpctypes.ErrDuplicateKey:
		return &engine.AlreadyExistsError{Message: e.Error()}
	}
	return e
}

func eventToString(e *etcd.Event) string {
	return fmt.Sprintf("%s: $v -> %v", e.Type, e.PrevKv, e.Kv)
}

func filterBySuffix(keys []*mvccpb.KeyValue, suffix string) ([]*mvccpb.KeyValue) {
	returnValue := []*mvccpb.KeyValue{}
	for _, key := range keys {
		if strings.Index(string(key.Key), suffix) == 0 {
			returnValue = append(returnValue, key)
		}
	}
	return returnValue
}

const (
	createA = "create"
	setA    = "set"
	deleteA = "delete"
	expireA = "expire"
	updateA = "update"
	cswapA  = "compareAndSwap"
	noTTL   = 0
)

type host struct {
	Name     string
	Settings hostSettings
}

type hostSettings struct {
	Default bool
	KeyPair []byte
	OCSP    engine.OCSPSettings
}
