package jsvm

import (
	"fmt"
	"sync"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
)

type loopPoolItem struct {
	mux  sync.Mutex
	busy bool
	loop *eventloop.EventLoop
}

type loopPool struct {
	mux     sync.RWMutex
	factory func() *eventloop.EventLoop
	items   []*loopPoolItem
}

func newLoopPool(size int, factory func() *eventloop.EventLoop) *loopPool {
	pool := &loopPool{
		factory: factory,
		items:   make([]*loopPoolItem, size),
	}

	for i := 0; i < size; i++ {
		loop := pool.factory()
		pool.items[i] = &loopPoolItem{loop: loop}
	}

	return pool
}

func (p *loopPool) run(call func(vm *goja.Runtime) error) error {
	p.mux.RLock()

	// try to find a free item
	var freeItem *loopPoolItem
	for _, item := range p.items {
		item.mux.Lock()
		if item.busy {
			item.mux.Unlock()
			continue
		}
		item.busy = true
		item.mux.Unlock()
		freeItem = item
		break
	}

	p.mux.RUnlock()

	// create a new one-off item if of all of the pool items are currently busy
	//
	// note: if turned out not efficient we may change this in the future
	// by adding the created item in the pool with some timer for removal
	if freeItem == nil {
		fmt.Println("Freee nul")
		var res error
		var w sync.WaitGroup
		w.Add(1)
		ll := p.factory()
		w.Done()
		w.Add(1)
		ll.RunOnLoop(func(r *goja.Runtime) {
			res = call(r)
			w.Done()
		})
		w.Wait()
		return res
	}

	var execErr error
	var w sync.WaitGroup
	// w.Add(1)
	// w.Done()
	w.Add(1)
	freeItem.loop.RunOnLoop(func(r *goja.Runtime) {
		execErr = call(r)
		w.Done()
	})
	w.Wait()

	// "free" the vm
	freeItem.mux.Lock()
	freeItem.busy = false
	freeItem.mux.Unlock()

	return execErr
}
