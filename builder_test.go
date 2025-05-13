// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"
)

type kv struct {
	name  string
	value int
}

func (i *kv) ID() string {
	return i.name
}

var testPayloadBase = &kv{
	name:  "data0",
	value: 5,
}

func deepcopy(item *kv) *kv {
	out := &kv{
		name:  item.name,
		value: item.value,
	}

	return out
}

func channelEdge[T any](c chan T) Edge[T, T] {
	return &edge[T, T]{
		send: func(ctx context.Context, data T) {
			c <- data
		},
		output: func() chan T {
			return c
		},
		metrics: newCollector(1000),
	}
}

func Benchmark_Test_New(b *testing.B) {
	channel := make(chan *kv)
	ctx := ContextWithOptions(context.Background(), OptionFIF0)

	e := channelEdge(channel)

	e = Map(
		ctx,
		e,
		func(m *kv) *kv {
			if m.ID() == "" {
				b.Errorf("packet missing name %v", m)
			}
			return m
		},
	)
	e = Map(
		ctx,
		e,
		func(m *kv) *kv {
			if m.ID() == "" {
				b.Errorf("packet missing name %v", m)
			}
			return m
		},
	)

	for n := 0; n < b.N; n++ {
		channel <- testPayloadBase

		<-e.Output()
	}
}

func Test_New(b *testing.T) {
	count := 10000
	channel := make(chan *kv)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithOptions(ctx)
	e := channelEdge(channel)

	go func() {
		for n := 0; n < count; n++ {
			channel <- &kv{
				name:  fmt.Sprintf("name%d", n),
				value: 11,
			}
		}
	}()

	e = Map(
		ctx,
		e,
		func(m *kv) *kv {
			return m
		},
	)

	e = Recurse(
		ctx,
		e,
		func(f Monad[*kv]) Monad[*kv] {
			return func(x *kv) *kv {
				if x.value < 3 {
					return &kv{x.name, 1}
				} else {
					return &kv{x.name, f(&kv{x.name, x.value - 1}).value + f(&kv{x.name, x.value - 2}).value}
				}
			}
		},
	)
	e = Memoize(
		ctx,
		e,
		func(f Monad[*kv]) Monad[*kv] {
			return func(x *kv) *kv {
				if x.value < 3 {
					return &kv{x.name, 1}
				} else {
					return &kv{x.name, f(&kv{x.name, x.value - 1}).value + f(&kv{x.name, x.value - 2}).value}
				}
			}
		},
		func(k *kv) string {
			return strconv.Itoa(k.value)
		},
	)

	left, bad := If(
		ctx,
		e,
		func(d *kv) bool {
			return true
		},
	)

	out := Map(
		ctx,
		left,
		func(payload *kv) int {
			return payload.value
		},
	)

	for n := 0; n < count; n++ {
		select {
		case x := <-out.Output():
			if x != 1779979416004714189 {
				b.Errorf("unexpected value %v", x)
			}
		case <-bad.Output():
			b.Errorf("should never reach this")
			b.FailNow()
		}
	}

	out.Metrics().Return()
	out.Metrics().Reset()

	cancel()

	<-time.After(10 * time.Millisecond)
}

func Test_New2(b *testing.T) {
	count := 100000
	channel := make(chan *kv)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithOptions(ctx, OptionFIF0, OptionBufferSize(1000))
	e := channelEdge(channel)

	go func() {
		for n := 0; n < count; n++ {
			channel <- deepcopy(testPayloadBase)
		}
	}()

	e = Map(
		ctx,
		e,
		func(m *kv) *kv {
			return m
		},
	)

	out, right := Tee(
		ctx,
		e,
		func(k *kv) (a *kv, b *kv) {
			return k, deepcopy(k)
		},
	)

	left, bad := If(
		ctx,
		right,
		func(d *kv) bool {
			return true
		},
	)

	bad2, right2 := If(
		ctx,
		left,
		func(d *kv) bool {
			return false
		},
	)

	out2, drop := Tee(
		ctx,
		right2,
		func(k *kv) (a *kv, b *kv) {
			return k, deepcopy(k)
		},
	)

	Drop(ctx, drop)

	for n := 0; n < 2*count; n++ {
		select {
		case <-out.Output():
		case <-out2.Output():
		case <-bad.Output():
			b.Errorf("should never reach this")
			b.FailNow()
		case <-bad2.Output():
			b.Errorf("should never reach this")
			b.FailNow()
		}
	}

	cancel()

	<-time.After(10 * time.Millisecond)
}

func Test_Panic(b *testing.T) {
	count := 100000
	channel := make(chan *kv)
	go func() {
		for n := 0; n < count; n++ {
			channel <- deepcopy(testPayloadBase)
		}
	}()

	e := channelEdge(channel)

	Map(
		context.Background(),
		e,
		func(m *kv) *kv {
			panic(fmt.Errorf("error"))
		},
	).Output()

	<-time.After(300 * time.Millisecond)
}

func Test_Loop(b *testing.T) {
	channel := make(chan *kv)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithOptions(ctx, OptionFIF0, OptionMetricWindowSize(100))
	e := channelEdge(channel)
	counter := 1
	counter2 := 1

	loopStart := Map(
		ctx,
		e,
		func(m *kv) *kv {
			return m
		},
	)

	loop, out := If(
		ctx,
		loopStart,
		func(a *kv) bool {
			counter++
			return counter%2 == 0
		},
	)

	nested, nestedOut := If(
		ctx,
		loop,
		func(a *kv) bool {
			counter2++
			return counter2%2 == 0
		},
	)

	loopEnd := Map(
		ctx,
		nested,
		func(m *kv) *kv {
			return m
		},
	)

	Send(
		ctx,
		nestedOut,
		loopStart,
	)

	Send(
		ctx,
		loopEnd,
		loop,
	)

	out.Metrics().Return()
	for range 10000 {
		channel <- deepcopy(testPayloadBase)
		<-out.Output()
	}


	cancel()

	<-time.After(100 * time.Millisecond)
}
