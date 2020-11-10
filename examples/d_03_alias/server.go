package main

import (
	"flag"
	"github.com/smallnest/rpcx/examples"
	"github.com/smallnest/rpcx/server"
	"github.com/smallnest/rpcx/serverplugin"
)

var (
	addr = flag.String("addr", "localhost:8972", "server address")
)

func main() {
	flag.Parse()

	a := serverplugin.NewAliasPlugin()
	a.Alias("a.b.c.D", "Times", "Arith", "Mul") //插件自定义方法
	b := NewMyPlugin()

	s := server.NewServer()
	s.Plugins.Add(a)
	s.Plugins.Add(b)
	s.RegisterName("Arith", new(examples.Arith), "")
	err := s.Serve("tcp", *addr)
	if err != nil {
		panic(err)
	}
}
