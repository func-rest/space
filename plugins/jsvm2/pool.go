package jsvm2

import (
	"sync"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dop251/goja_nodejs/require"
)

type poolItem struct {
	mux  sync.Mutex
	busy bool
	// vm   *goja.Runtime
	loop *eventloop.EventLoop
}

type vmsPool struct {
	mux     sync.RWMutex
	factory func(r *goja.Runtime)
	items   []*poolItem
	reg     *require.Registry
}

// newPool creates a new pool with pre-warmed vms generated from the specified factory.
func newPool(size int, factory func(r *goja.Runtime), reg *require.Registry) *vmsPool {
	pool := &vmsPool{
		factory: factory,
		items:   make([]*poolItem, size),
	}

	for i := 0; i < size; i++ {
		// vm := pool.factory()

		pool.items[i] = &poolItem{
			// vm: vm,
			loop: eventloop.NewEventLoop(eventloop.WithRegistry(reg))}
		pool.items[i].loop.Start()
		pool.items[i].loop.RunOnLoop(func(r *goja.Runtime) {
			factory(r)
		})
		defer pool.items[i].loop.Stop()
	}

	return pool
}

// run executes "call" with a vm created from the pool
// (either from the buffer or a new one if all buffered vms are busy)
func (p *vmsPool) run(call func(vm *goja.Runtime) error) error {
	p.mux.RLock()

	// try to find a free item
	var freeItem *poolItem
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
		loop := eventloop.NewEventLoop(eventloop.WithRegistry(p.reg))
		loop.Start()
		loop.RunOnLoop(func(r *goja.Runtime) {
			p.factory(r)

			call(r)
		})
		defer loop.Stop()

	}

	var execErr error
	ch := make(chan error)

	freeItem.loop.RunOnLoop(func(vm *goja.Runtime) {
		err := call(vm)

		if err != nil {
			ch <- err
		}
	})
	execErr = <-ch
	if execErr != nil {
		return execErr
	}

	freeItem.mux.Lock()
	freeItem.busy = false
	freeItem.mux.Unlock()

	return execErr
}
