// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import (
	"context"
	"fmt"
	"time"
)

// Map applies a function to the payload
func Map[T, U, V any](ctx context.Context, m Edge[T, U], fn Transformation[U, V]) Edge[U, V] {
	e := fn.Edge(ctx)

	go transfer(ctx, m, wrap(e))

	return e
}

// Recurse applies a recursive function to the payload through a Y Combinator.
func Recurse[T, U any](ctx context.Context, m Edge[T, U], fn Monad[Monad[U]]) Edge[U, U] {
	g := func(h recursiveBaseFn[U]) Monad[U] {
		return func(payload U) U {
			return fn(h(h))(payload)
		}
	}

	return Map(ctx, m, Transformation[U, U](g(g)))
}

// Memoize applies a recursive function to the payload through a Y Combinator
// and memoizes the results based on the index func.
func Memoize[T, U any](ctx context.Context, m Edge[T, U], fn Monad[Monad[U]], index func(U) string) Edge[U, U] {
	g := func(h memoizedBaseFn[U], m map[string]U) Monad[U] {
		return func(payload U) U {
			id := index(payload)
			if v, ok := m[id]; ok {
				return v
			}

			m[id] = fn(h(h, m))(payload)
			return m[id]
		}
	}
	p := Monad[U](func(payload U) U {
		m := map[string]U{}
		return g(g, m)(payload)
	})

	return Map(ctx, m, Transformation[U, U](p))
}

// If splits the data into multiple stream branches
func If[T, U any](ctx context.Context, m Edge[T, U], fn Filter[U]) (left, right Edge[U, U]) {
	return split(ctx, m, fn.Edge)
}

// Tee duplicates the data into multiple stream branches. The payload/vertexes are
// responsible for concurrent read/write controls
func Tee[T, U any](ctx context.Context, m Edge[T, U], fn func(U) (a, b U)) (left, right Edge[U, U]) {
	var v vertex[U] = func(left, right chan U) func(ctx context.Context, data U) {
		return func(_ context.Context, data U) {
			a, b := fn(data)
			left <- a
			right <- b
		}
	}

	return split(ctx, m, v.edge)
}

// Send is a function used for looping/cycles
func Send[T, U, V any](ctx context.Context, m Edge[T, U], e Edge[U, V]) {
	go transfer(ctx, m, wrap(e))
}

// Drop is a function used for dropping data
// and not sending it to the next edge.
func Drop[T, U any](ctx context.Context, m Edge[T, U]) {
	go transfer(ctx, m, func(ctx context.Context, data U) {})
}

func split[T, U any](ctx context.Context, m Edge[T, U], fn func(context.Context) (Edge[U, U], Edge[U, U])) (left, right Edge[U, U]) {
	l, r := fn(ctx)
	go transfer(ctx, m, wrap(l))
	return l, r
}

func transfer[T, U any](ctx context.Context, a Edge[T, U], fn func(ctx context.Context, data U)) {
	fifo := fromContext(ctx).fifo
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-a.Output():
			if fifo {
				fn(ctx, data)
			} else {
				go fn(ctx, data)
			}
		}
	}
}

func wrap[T, U any](e Edge[T, U]) func(ctx context.Context, data T) {
	return func(ctx context.Context, data T) {
		defer recoverAndRecord(time.Now(), e)
		e.Send(ctx, data)
	}
}

func recoverAndRecord[T, U any](start time.Time, e Edge[T, U]) (err error) {
	if r := recover(); r != nil {
		err = fmt.Errorf("panic: %v", r)
	}
	e.Metrics().Record(time.Since(start), err)
	
	return
}
