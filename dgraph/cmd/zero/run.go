/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package zero

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/dgraph/conn"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/raftwal"
	"github.com/dgraph-io/dgraph/x"
	"github.com/spf13/cobra"
)

type options struct {
	bindall           bool
	myAddr            string
	portOffset        int
	nodeId            uint64
	numReplicas       int
	peer              string
	w                 string
	rebalanceInterval time.Duration
}

var opts options

var Zero x.SubCommand

func init() {
	Zero.Cmd = &cobra.Command{
		Use:   "zero",
		Short: "Run Dgraph zero server",
		Long: `
A Dgraph zero instance manages the Dgraph cluster.  Typically, a single Zero
instance is sufficient for the cluster; however, one can run multiple Zero
instances to achieve high-availability.
`,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Zero.Conf).Stop()
			run()
		},
	}
	Zero.EnvPrefix = "DGRAPH_ZERO"

	flag := Zero.Cmd.Flags()
	flag.String("my", "",
		"addr:port of this server, so other Dgraph servers can talk to this.")
	flag.IntP("port_offset", "o", 0,
		"Value added to all listening port numbers. [Grpc=5080, HTTP=6080]")
	flag.Uint64("idx", 1, "Unique node index for this server.")
	flag.Int("replicas", 1, "How many replicas to run per data shard."+
		" The count includes the original shard.")
	flag.String("peer", "", "Address of another dgraphzero server.")
	flag.StringP("wal", "w", "zw", "Directory storing WAL.")
	flag.Duration("rebalance_interval", 8*time.Minute, "Interval for trying a predicate move.")
	flag.Bool("telemetry", true, "Send anonymous telemetry data to Dgraph devs.")
}

func setupListener(addr string, port int, kind string) (listener net.Listener, err error) {
	laddr := fmt.Sprintf("%s:%d", addr, port)
	fmt.Printf("Setting up %s listener at: %v\n", kind, laddr)
	return net.Listen("tcp", laddr)
}

type state struct {
	node *node
	rs   *conn.RaftServer
	zero *Server
}

func (st *state) serveGRPC(l net.Listener, wg *sync.WaitGroup, store *raftwal.DiskStorage) {
	s := grpc.NewServer(
		grpc.MaxRecvMsgSize(x.GrpcMaxSize),
		grpc.MaxSendMsgSize(x.GrpcMaxSize),
		grpc.MaxConcurrentStreams(1000))

	rc := pb.RaftContext{Id: opts.nodeId, Addr: opts.myAddr, Group: 0}
	m := conn.NewNode(&rc, store)
	st.rs = &conn.RaftServer{Node: m}

	st.node = &node{Node: m, ctx: context.Background(), stop: make(chan struct{})}
	st.zero = &Server{NumReplicas: opts.numReplicas, Node: st.node}
	st.zero.Init()
	st.node.server = st.zero

	pb.RegisterZeroServer(s, st.zero)
	pb.RegisterRaftServer(s, st.rs)

	go func() {
		defer wg.Done()
		err := s.Serve(l)
		log.Printf("gRpc server stopped : %s", err.Error())
		st.node.stop <- struct{}{}

		// Attempt graceful stop (waits for pending RPCs), but force a stop if
		// it doesn't happen in a reasonable amount of time.
		done := make(chan struct{})
		const timeout = 5 * time.Second
		go func() {
			s.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			log.Printf("Stopping grpc gracefully is taking longer than %v."+
				" Force stopping now. Pending RPCs will be abandoned.", timeout)
			s.Stop()
		}
	}()
}

func run() {
	x.PrintVersion()
	opts = options{
		bindall:           Zero.Conf.GetBool("bindall"),
		myAddr:            Zero.Conf.GetString("my"),
		portOffset:        Zero.Conf.GetInt("port_offset"),
		nodeId:            uint64(Zero.Conf.GetInt("idx")),
		numReplicas:       Zero.Conf.GetInt("replicas"),
		peer:              Zero.Conf.GetString("peer"),
		w:                 Zero.Conf.GetString("wal"),
		rebalanceInterval: Zero.Conf.GetDuration("rebalance_interval"),
	}

	if opts.numReplicas < 0 || opts.numReplicas%2 == 0 {
		log.Fatalf("ERROR: Number of replicas must be odd for consensus. Found: %d",
			opts.numReplicas)
	}

	if Zero.Conf.GetBool("expose_trace") {
		trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
			return true, true
		}
	}
	grpc.EnableTracing = false

	addr := "localhost"
	if opts.bindall {
		addr = "0.0.0.0"
	}
	if len(opts.myAddr) == 0 {
		opts.myAddr = fmt.Sprintf("localhost:%d", x.PortZeroGrpc+opts.portOffset)
	}
	grpcListener, err := setupListener(addr, x.PortZeroGrpc+opts.portOffset, "grpc")
	if err != nil {
		log.Fatal(err)
	}
	httpListener, err := setupListener(addr, x.PortZeroHTTP+opts.portOffset, "http")
	if err != nil {
		log.Fatal(err)
	}

	// Open raft write-ahead log and initialize raft node.
	x.Checkf(os.MkdirAll(opts.w, 0700), "Error while creating WAL dir.")
	kvOpt := badger.LSMOnlyOptions
	kvOpt.SyncWrites = true
	kvOpt.Truncate = true
	kvOpt.Dir = opts.w
	kvOpt.ValueDir = opts.w
	kvOpt.ValueLogFileSize = 64 << 20
	kv, err := badger.Open(kvOpt)
	x.Checkf(err, "Error while opening WAL store")
	defer kv.Close()
	store := raftwal.Init(kv, opts.nodeId, 0)

	var wg sync.WaitGroup
	wg.Add(3)
	// Initialize the servers.
	var st state
	st.serveGRPC(grpcListener, &wg, store)
	st.serveHTTP(httpListener, &wg)

	http.HandleFunc("/state", st.getState)
	http.HandleFunc("/removeNode", st.removeNode)
	http.HandleFunc("/moveTablet", st.moveTablet)
	http.HandleFunc("/assignIds", st.assignUids)

	// This must be here. It does not work if placed before Grpc init.
	x.Check(st.node.initAndStartNode())

	if Zero.Conf.GetBool("telemetry") {
		go st.zero.periodicallyPostTelemetry()
	}

	sdCh := make(chan os.Signal, 1)
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer wg.Done()
		<-sdCh
		fmt.Println("Shutting down...")
		// Close doesn't close already opened connections.
		httpListener.Close()
		grpcListener.Close()
		close(st.zero.shutDownCh)
		st.node.trySnapshot(0)
	}()

	fmt.Println("Running Dgraph zero...")
	wg.Wait()
	fmt.Println("All done.")
}
