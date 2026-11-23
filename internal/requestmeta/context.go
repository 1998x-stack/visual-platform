// Package requestmeta propagates a server-generated request ID through
// business services and audit writes without trusting client headers.
package requestmeta

import "context"

type key struct{}
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx,key{},id)
}
func ID(ctx context.Context) string {
	id,_:=ctx.Value(key{}).(string)
	return id
}
