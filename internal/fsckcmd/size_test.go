package fsckcmd

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestDbgSizes(t *testing.T) {
	fmt.Println("objEntry", unsafe.Sizeof(objEntry{}))
	fmt.Println("slot", unsafe.Sizeof(slot{}))
	fmt.Println("objShard", unsafe.Sizeof(objShard{}))
	fmt.Println("edge", unsafe.Sizeof(edge(0)))
	t.Fatal("dbg")
}
