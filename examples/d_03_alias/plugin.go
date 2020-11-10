package main

import (
	"context"
	"fmt"
)

type MyPlugin struct {
}

func NewMyPlugin() *MyPlugin {
	return &MyPlugin{}
}

func (*MyPlugin) PreCall(ctx context.Context, serviceName, methodName string, args interface{}) (interface{}, error) {

	fmt.Println("MyPlugin -- PreCall:", "--serviceName:", serviceName, "--methodName:", methodName, "--args:", args)

	return args, nil
}

func (*MyPlugin) PostCall(ctx context.Context, serviceName, methodName string, args, reply interface{}) (interface{}, error) {

	fmt.Println("MyPlugin -- PostCall:", "--serviceName:", serviceName, "--methodName:", methodName, "--args:", args)

	return reply, nil
}
