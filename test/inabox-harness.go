package test

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	u "github.com/araddon/gou"
	"github.com/disney/quanta/core"
	"github.com/disney/quanta/custom/functions"
	"github.com/disney/quanta/qlbridge/expr"
	"github.com/disney/quanta/qlbridge/expr/builtins"
	"github.com/disney/quanta/qlbridge/schema"
	admin "github.com/disney/quanta/quanta-admin-lib"
	proxy "github.com/disney/quanta/quanta-proxy-lib"
	"github.com/disney/quanta/rbac"
	"github.com/disney/quanta/server"
	"github.com/disney/quanta/shared"
	"github.com/disney/quanta/sink"
	"github.com/disney/quanta/source"
	"github.com/hashicorp/consul/api"
)

// some tests start a cluster and must listen on port 4000
// this is a mutex to ensure that only one test at a time can listen on port 4000
var acquirePort4000 sync.Mutex

// tests will time out so run like this:
// go test -timeout 10m

func StartNode(nodeStart int) (*server.Node, error) {

	Version := "v0.0.1"
	Build := "2006-01-01"

	environment := "DEV"
	logLevel := "DEBUG"

	shared.InitLogging(logLevel, environment, "Data-Node", Version, "Quanta")

	index := nodeStart
	{
		hashKey := "quanta-node-" + strconv.Itoa(index)
		dataDir := "../test/localClusterData/" + hashKey + "/data"
		bindAddr := "127.0.0.1"
		port := 4010 + index

		consul := bindAddr + ":8500"

		memLimit := 0

		// Create /bitmap data directory
		fmt.Printf("Creating bitmap data directory: %s", dataDir+"/bitmap")
		if _, err := os.Stat(dataDir + "/bitmap"); err != nil {
			err = os.MkdirAll(dataDir+"/bitmap", 0777)
			if err != nil {
				u.Errorf("[node: Cannot initialize endpoint config: error: %s", err)
			}
		}

		u.Infof("Node identifier '%s'", hashKey)

		u.Infof("Connecting to Consul at: [%s] ...\n", consul)
		consulClient, err := api.NewClient(&api.Config{Address: consul})
		if err != nil {
			u.Errorf("Is the consul agent running?")
			log.Fatalf("[node: Cannot initialize endpoint config: error: %s", err)
		}

		newNodeName := hashKey
		// newNodeName = "quanta"
		m, err := server.NewNode(fmt.Sprintf("%v:%v", Version, Build), int(port), bindAddr, dataDir, newNodeName, consulClient)
		if err != nil {
			u.Errorf("[node: Cannot initialize node config: error: %s", err)
		}
		m.ServiceName = "quanta" // not hashKey. Do we need this? (atw)
		m.IsLocalCluster = true

		kvStore := server.NewKVStore(m)
		m.AddNodeService(kvStore)

		search := server.NewStringSearch(m)
		m.AddNodeService(search)

		bitmapIndex := server.NewBitmapIndex(m, int(memLimit))
		m.AddNodeService(bitmapIndex)

		// load the table schema from the file system manually here

		// table := "cities"
		// shared.LoadSchema("../test/testdata/config", table, consulClient)
		// // ? m.TableCache.TableCache[table] = t

		// table = "cityzip"
		// shared.LoadSchema("../test/testdata/config", table, consulClient)

		// table = "dmltest"
		// shared.LoadSchema("../test/testdata/config", table, consulClient)

		// Start listening endpoint
		m.Start()

		start := time.Now()
		err = m.InitServices()
		elapsed := time.Since(start)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Node initialized in %v.", elapsed)

		go func() {
			joinName := "quanta"   // this is the name for a cluster of nodes was "quanta-node"
			err = m.Join(joinName) // this does not return
			if err != nil {
				u.Errorf("[node: Cannot initialize endpoint config: error: %s", err)
			}
			fmt.Println("StartNodes returned from join")

			<-m.Stop
			select {
			case err = <-m.Err:
			default:
			}
			if err != nil {
				u.Errorf("[node: Cannot initialize endpoint config: error: %s", err)
			}
		}()
		return m, nil
	}
}

type LocalProxyControl struct {
	Stop chan bool
}

func StartProxy(count int, testConfigPath string) *LocalProxyControl {

	localProxy := &LocalProxyControl{}

	localProxy.Stop = make(chan bool)

	fmt.Println("Starting proxy")

	proxy.SetupCounters()
	proxy.Init()

	logging := "DEBUG"
	environment := "DEV"
	Version := "1.0.0"
	proxy.ConsulAddr = "127.0.0.1:8500"
	// cognito url for token service publicKeyURL := "" // unused
	proxy.QuantaPort = 4010
	proxyHostPort := 4000

	// region := "us-east-1"

	if strings.ToUpper(logging) == "DEBUG" || strings.ToUpper(logging) == "TRACE" {
		if strings.ToUpper(logging) == "TRACE" {
			expr.Trace = true
		}
		u.SetupLogging("debug")
	} else {
		shared.InitLogging(logging, environment, "Proxy", Version, "Quanta")
	}

	log.Printf("Connecting to Consul at: [%s] ...\n", proxy.ConsulAddr)
	consulConfig := &api.Config{Address: proxy.ConsulAddr}
	errx := shared.RegisterSchemaChangeListener(consulConfig, proxy.SchemaChangeListener)
	if errx != nil {
		u.Error(errx)
		os.Exit(1)
	}

	fmt.Println("Proxy RegisterSchemaChangeListener done")

	poolSize := 3

	// If the pool size is not configured then set it to the number of available CPUs
	// this is weird atw
	sessionPoolSize := poolSize
	if sessionPoolSize == 0 {
		sessionPoolSize = runtime.NumCPU()
		log.Printf("Session Pool Size not set, defaulting to number of available CPUs = %d", sessionPoolSize)
	} else {
		log.Printf("Session Pool Size = %d", sessionPoolSize)
	}

	// Match 2 or more whitespace chars inside string
	reWhitespace := regexp.MustCompile(`[\s\p{Zs}]{2,}`)
	_ = reWhitespace

	// load all of our built-in functions
	builtins.LoadAllBuiltins()
	sink.LoadAll()      // Register output sinks
	functions.LoadAll() // Custom functions

	// start cloud watch or prometheus metrics?

	fmt.Println("Proxy before NewQuantaSource")

	// Construct Quanta source
	tableCache := core.NewTableCacheStruct() // is this right? delete this?

	// configDir := "../test/testdata" // gets: ../test/testdata/config/schema.yaml
	// FIXME: empty configDir panics
	configDir := testConfigPath
	var err error                                                                                                       // this fails when run from test?
	proxy.Src, err = source.NewQuantaSource(tableCache, configDir, proxy.ConsulAddr, proxy.QuantaPort, sessionPoolSize) // do we really want this here?
	if err != nil {
		u.Error(err)
	}
	fmt.Println("Proxy after NewQuantaSource")

	schema.RegisterSourceAsSchema("quanta", proxy.Src)

	fmt.Println("Proxy starting to listen. ")

	// Start server endpoint
	portStr := strconv.FormatInt(int64(proxyHostPort), 10)
	listener, err := net.Listen("tcp", "0.0.0.0:"+portStr)
	if err != nil {
		panic(err.Error())
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				u.Errorf(err.Error())
				return
			}
			go proxy.OnConn(conn)
		}
	}()

	go func(localProxy *LocalProxyControl) {

		for range localProxy.Stop {
			fmt.Println("Stopping proxy")
			proxy.Src.Close()
			listener.Close()
		}

	}(localProxy)

	return localProxy
}

func IsLocalRunning() bool {

	result := "[]"
	res, err := http.Get("http://localhost:8500/v1/health/service/quanta") // was quanta-node
	if err == nil {
		resBody, err := io.ReadAll(res.Body)
		if err == nil {
			result = string(resBody)
		}
	} else {
		fmt.Println("is consul not running?", err)
	}
	// fmt.Println("result:", result)

	// the three cases are 1) what cluster? 2) used to have one but now its critical 3) Here's the health.
	isNotRunning := strings.HasPrefix(result, "[]") || strings.Contains(result, "critical")

	return !isNotRunning
}

type ClusterLocalState struct {
	m0                  *server.Node
	m1                  *server.Node
	m2                  *server.Node
	proxyControl        *LocalProxyControl
	weStartedTheCluster bool
	proxyConnect        *ProxyConnect // for sql runner
	db                  *sql.DB
}

func StartNodes(state *ClusterLocalState) {

	state.m0, _ = StartNode(0)
	state.m1, _ = StartNode(1)
	state.m2, _ = StartNode(2)
}

func (state *ClusterLocalState) StopNodes() {

	cmd := admin.ShutdownCmd{}
	cmd.NodeIP = "all" // this would probably work

	ctx := admin.Context{ConsulAddr: consulAddress,
		Port:  4000,
		Debug: true}

	cmd.NodeIP = "127.0.0.1:4010"
	cmd.Run(&ctx)

	cmd.NodeIP = "127.0.0.1:4011"
	cmd.Run(&ctx)

	cmd.NodeIP = "127.0.0.1:4012"
	cmd.Run(&ctx)
}

func (state *ClusterLocalState) Release() {
	if state.weStartedTheCluster {
		state.proxyControl.Stop <- true
		time.Sleep(100 * time.Millisecond)
		state.StopNodes()
		time.Sleep(100 * time.Millisecond)
	}
}

func WaitForLocalActive(state *ClusterLocalState) {
	for state.m0.State != server.Active || state.m1.State != server.Active || state.m2.State != server.Active {
		time.Sleep(100 * time.Millisecond)
		//fmt.Println("Waiting for nodes...", m2.State)
	}
}

// Ensure_cluster checks to see if there already is a cluster and
// starts a local one as needed.
// This depends on having consul on port 8500
func Ensure_cluster() *ClusterLocalState {
	var state = &ClusterLocalState{}

	var proxyConnect ProxyConnect
	proxyConnect.Host = "127.0.0.1"
	proxyConnect.User = "MOLIG004"
	proxyConnect.Password = ""
	proxyConnect.Port = "4000"
	proxyConnect.Database = "quanta"

	state.proxyConnect = &proxyConnect

	isNotRunning := !IsLocalRunning()
	if isNotRunning {
		// start the cluster
		StartNodes(state)

		WaitForLocalActive(state)

		// atw FIXME get rid of this config
		// configDir := "../test/testdata/config"
		configDir := ""
		state.proxyControl = StartProxy(1, configDir)

		state.weStartedTheCluster = true
	} else {
		state.weStartedTheCluster = false
	}

	// need to sort this out and just have one

	conn := shared.NewDefaultConnection()
	err := conn.Connect(nil)
	check(err)
	defer conn.Disconnect()

	sharedKV := shared.NewKVStore(conn)

	ctx, err := rbac.NewAuthContext(sharedKV, "USER001", true)
	check(err)
	err = ctx.GrantRole(rbac.DomainUser, "USER001", "quanta", true)
	check(err)

	ctx, err = rbac.NewAuthContext(sharedKV, "MOLIG004", true)
	check(err)
	err = ctx.GrantRole(rbac.SystemAdmin, "MOLIG004", "quanta", true)
	check(err)

	state.db, err = state.proxyConnect.ProxyConnectConnect()
	check(err)
	return state
}

func check(err error) {
	if err != nil {
		fmt.Println("check err", err)
		panic(err.Error())
	}
}
