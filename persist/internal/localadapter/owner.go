// Package localadapter 将本地缓存引擎适配为相互隔离的业务和 HTTP 存储视图。
package localadapter

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var (
	// ErrInvalidArgument 表示构造参数、TTL 或键长度不合法。
	ErrInvalidArgument = errors.New("cacheadapter: invalid argument")
	// ErrClosed 表示视图或其 Owner 已开始关闭。
	ErrClosed = errors.New("cacheadapter: closed")
	// ErrCorruptEntry 表示存储记录格式损坏。
	ErrCorruptEntry = errors.New("cacheadapter: corrupt entry")
)

// Engine 是原生适配器在受控操作门内提供的最小能力。
type Engine interface {
	Get(string) ([]byte, error)
	Set(string, []byte, time.Duration) (bool, error)
	Delete(string) error
	Walk(func(string, []byte)) error
	Close() error
}

// Owner 串行管理一个原生实例。每个原生实例只能建立一个 Owner；禁止旁路操作。
// New 系列拥有实例，Wrap 系列借用实例。零值不可用。
type Owner struct {
	gate     chan struct{}
	state    atomic.Uint32 // 0: active, 1: closing, 2: closed
	engine   Engine
	owned    bool
	stores   map[string]*Store
	sequence uint64
	closeErr error
}

// NewOwner 绑定已校验的原生实现；仅供同仓适配模块装配。
func NewOwner(e Engine, owned bool) *Owner {
	o := &Owner{gate: make(chan struct{}, 1), engine: e, owned: owned, stores: make(map[string]*Store)}
	o.gate <- struct{}{}
	return o
}

func (o *Owner) acquire(ctx context.Context, closing bool) error {
	if o == nil || o.gate == nil || ctx == nil {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !closing && o.state.Load() != 0 {
		return ErrClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.gate:
	}
	if err := ctx.Err(); err != nil {
		o.release()
		return err
	}
	if !closing && o.state.Load() != 0 {
		o.release()
		return ErrClosed
	}
	return nil
}

func (o *Owner) release() { o.gate <- struct{}{} }

// Close 停止新操作并释放拥有的实例。等待操作权时响应 ctx；已进入的原生
// Close 不可中断。超时后保持 closing，可用新 ctx 再次等待。借用实例不被关闭。
func (o *Owner) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if ctx == nil || o.gate == nil {
		return ErrInvalidArgument
	}
	o.state.CompareAndSwap(0, 1)
	if err := o.acquire(ctx, true); err != nil {
		return err
	}
	defer o.release()
	if o.state.Load() == 2 {
		return o.closeErr
	}
	if o.owned {
		o.closeErr = o.engine.Close()
	}
	o.engine = nil
	o.state.Store(2)
	return o.closeErr
}
