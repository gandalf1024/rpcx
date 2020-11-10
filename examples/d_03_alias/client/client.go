package main

import (
	"context"
	"flag"
	"github.com/smallnest/rpcx/client"
	"github.com/smallnest/rpcx/examples"
	"log"
	"time"
)

var (
	addr = flag.String("addr", "localhost:8972", "server address")
)

func main() {
	flag.Parse()

	d := client.NewPeer2PeerDiscovery("tcp@"+*addr, "")

	option := client.DefaultOption
	option.ConnectTimeout = 10 * time.Second

	xclient := client.NewXClient("a.b.c.D", client.Failtry, client.RandomSelect, d, option)
	defer xclient.Close()

	args := &examples.Args{
		A: 10,
		B: 20,
	}

	reply := &examples.Reply{}
	err := xclient.Call(context.Background(), "Times", args, reply)
	if err != nil {
		log.Fatalf("failed to call: %v", err)
	}

	log.Printf("%d * %d = %d", args.A, args.B, reply.C)

}
