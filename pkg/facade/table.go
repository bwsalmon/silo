package facade

// Table is a facade's method table (facades.md, "The pipeline every
// facade shares", step 3). The rows are the service's own, in its
// tables.go; the lookup is shared.
type Table[H any] struct {
	// Resources are the resource types the facade serves.
	Resources map[string]bool
	// Refused are resource types the facade knows and refuses every
	// method of: a refusal naming the method, not "not found".
	Refused map[string]bool
	// Methods are the handlers, by the service's own method key.
	Methods map[string]H
}

// Known reports whether the facade knows resource at all. A call on one
// it doesn't is answered as not found.
func (t Table[H]) Known(resource string) bool {
	return t.Resources[resource] || t.Refused[resource]
}

// Lookup returns the handler for method, a call on resource. ok is
// false for a method the table doesn't serve, or one on a refused
// resource type, which the facade refuses.
func (t Table[H]) Lookup(resource, method string) (h H, ok bool) {
	if t.Refused[resource] {
		return h, false
	}
	h, ok = t.Methods[method]
	return h, ok
}
