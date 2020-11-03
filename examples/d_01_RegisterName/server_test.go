package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

type methodType struct {
	sync.Mutex
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
}

type functionType struct {
	sync.Mutex
	fn        reflect.Value
	ArgType   reflect.Type
	ReplyType reflect.Type
}

type service struct {
	name     string
	rcvr     reflect.Value
	typ      reflect.Type
	method   map[string]*methodType
	function map[string]*functionType
}

var req = `{"A":10,"B":20}`

func Test_Reflect(t *testing.T) {
	a := new(Arith)
	service := new(service)           //初始化service实例
	service.typ = reflect.TypeOf(a)   //获取结构体类型
	service.rcvr = reflect.ValueOf(a) //获取
	sname := reflect.Indirect(service.rcvr).Type().Name()
	service.name = sname
	service.method = suitableMethods(service.typ)

	mtype := service.method["Mul"]
	argv := instance(mtype.ArgType)
	replyv := instance(mtype.ReplyType)

	err := Decode([]byte(req), argv)
	if err != nil {
		panic(err)
	}

	if mtype.ArgType.Kind() != reflect.Ptr {
		err = service.call(context.Background(), mtype, reflect.ValueOf(argv).Elem(), reflect.ValueOf(replyv))
	} else {
		err = service.call(context.Background(), mtype, reflect.ValueOf(argv), reflect.ValueOf(replyv))
	}

	fmt.Println(err)
}

func suitableMethods(typ reflect.Type) map[string]*methodType {
	methods := make(map[string]*methodType) //map[别名]方法
	for m := 0; m < typ.NumMethod(); m++ {  //反射便利所有方法
		method := typ.Method(m) //获取单个方法
		mtype := method.Type    //获取方法类型
		mname := method.Name    //获取方法名称

		replyType := mtype.In(3)
		argType := mtype.In(2)
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

func instance(t reflect.Type) interface{} {
	var argv reflect.Value

	if t.Kind() == reflect.Ptr { // reply must be ptr
		argv = reflect.New(t.Elem())
	} else {
		argv = reflect.New(t)
	}

	return argv.Interface()
}

func (s *service) call(ctx context.Context, mtype *methodType, argv, replyv reflect.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println(r)
		}
	}()

	function := mtype.method.Func
	//反射执行方法
	returnValues := function.Call([]reflect.Value{s.rcvr, reflect.ValueOf(ctx), argv, replyv})
	errInter := returnValues[0].Interface()
	if errInter != nil {
		return errInter.(error)
	}

	return nil
}

func Decode(data []byte, i interface{}) error {
	d := json.NewDecoder(bytes.NewBuffer(data))
	d.UseNumber()
	return d.Decode(i)
}
