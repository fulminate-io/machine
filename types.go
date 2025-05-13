// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import (
	"context"
)

// Transformation is a function that is applied to data and used for transformations
type Transformation[T, U any] func(d T) U

// Monad is a function that is applied to data and used for transformations
type Monad[T any] func(d T) T

// Filter is a function that can be used to filter the data.
type Filter[T any] func(d T) bool

// Edge is an interface that is used for transferring data between vertices
type Edge[T, U any] interface {
	Output() chan U
	Send(ctx context.Context, data T)
	Metrics() *MetricCollector
}

type edge[T, U any] struct {
	send    func(ctx context.Context, data T)
	output  func() chan U
	metrics *MetricCollector
}

// Option is used to configure the machine
type Option interface {
	apply(*config)
}

type option struct {
	fn func(*config)
}

type vertex[T any] func(left, right chan T) func(ctx context.Context, data T)
type recursiveBaseFn[T any] func(recursiveBaseFn[T]) Monad[T]
type memoizedBaseFn[T any] func(h memoizedBaseFn[T], m map[string]T) Monad[T]

// Edge is a function that is used to create an edge from the transformation
func (x Transformation[T, U]) Edge(ctx context.Context) Edge[T, U] {
	config := fromContext(ctx)
	output := make(chan U)
	return &edge[T, U]{
		send:    func(_ context.Context, data T) { output <- x(data) },
		output:  func() chan U { return output },
		metrics: newCollector(config.metricsWindowSize),
	}
}

// Edge is a function that is used to create an edge from the filter
func (x Filter[T]) Edge(ctx context.Context) (Edge[T, T], Edge[T, T]) {
	return x.vertex().edge(ctx)
}

func (x Filter[T]) vertex() vertex[T] {
	return func(left, right chan T) func(ctx context.Context, data T) {
		return func(_ context.Context, data T) {
			if x(data) {
				left <- data
			} else {
				right <- data
			}
		}
	}
}

func (e *edge[T, U]) Output() chan U {
	return e.output()
}

func (e *edge[T, U]) Send(ctx context.Context, data T) {
	e.send(ctx, data)
}

func (e *edge[T, U]) Metrics() *MetricCollector {
	return e.metrics
}

func (o *option) apply(c *config) {
	o.fn(c)
}

type keyType struct{}

var key keyType

// ContextWithOptions returns a new context with the options applied
func ContextWithOptions(ctx context.Context, options ...Option) context.Context {
	config := newconfig(options...)
	return context.WithValue(ctx, key, config)
}

func fromContext(ctx context.Context) *config {
	if v := ctx.Value(key); v != nil {
		return v.(*config)
	}
	return newconfig()
}

// OptionFIF0 controls the processing order of the datas
// If set to true the system will wait for one data
// to be processed before starting the next.
var OptionFIF0 Option = &option{func(c *config) { c.fifo = true }}

// OptionBufferSize sets the buffer size on the edge channels between the
// vertices, this setting can be useful when processing large amounts
// of data with FIFO turned on.
func OptionBufferSize(size int) Option {
	return &option{func(c *config) { c.bufferSize = size }}
}

// OptionMetricWindowSize sets the size of the slice holding duration metrics
func OptionMetricWindowSize(size uint64) Option {
	return &option{func(c *config) { c.metricsWindowSize = size }}
}

// config is used to configure the Edges
type config struct {
	fifo              bool
	bufferSize        int
	metricsWindowSize uint64
}

func newconfig(options ...Option) *config {
	c := &config{
		fifo:              false,
		bufferSize:        0,
		metricsWindowSize: 10000,
	}

	for _, option := range options {
		option.apply(c)
	}

	return c
}

func (x vertex[T]) edge(ctx context.Context) (Edge[T, T], Edge[T, T]) {
	config := fromContext(ctx)
	left := make(chan T)
	right := make(chan T)
	return &edge[T, T]{
			send:    x(left, right),
			output:  func() chan T { return left },
			metrics: newCollector(config.metricsWindowSize),
		}, &edge[T, T]{
			send:    x(left, right),
			output:  func() chan T { return right },
			metrics: newCollector(config.metricsWindowSize),
		}
}
