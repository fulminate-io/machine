package main

// Order is the datum flowing through the orders flow.
type Order struct {
	Kind  string
	Total int
}

// Poll is the source: it yields the next order to process.
func Poll() Order {
	return Order{Kind: "card", Total: 100}
}

// Enrich fills in anything missing on an order before routing.
func Enrich(o Order) Order {
	if o.Kind == "" {
		o.Kind = "unknown"
	}
	return o
}

// Count bumps the accepted counter and passes the order through.
func Count(o Order, accepted *int) Order {
	*accepted++
	return o
}

// Store sinks an accepted order.
func Store(o Order) error {
	return nil
}

// Reject sinks an order that was routed away from billing.
func Reject(o Order) error {
	return nil
}
