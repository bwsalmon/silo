// Package ledger is silod's quota ledger: regional quota, global
// counts, allowed regions and the model budget, handed down the silo
// tree as a hard partition (docs/silo/allocators.md, SV-27 to SV-37;
// docs/silo/agents.md, "The allocator", SV-109).
//
// # The shape
//
// Kind and class are data (docs/silo/custom-allocators.md, "What v1
// must already allow"). A class is a row of the class table, keyed by
// (kind, name), with a unit, a sort (held or metered), a scope
// (regional or global) and a period. Migration 0002 seeds kind gce with
// every class in allocators.md, and kind model with class usd. Nothing
// here switches on a class name: a class is looked up, and one that
// isn't in the table is INVALID_ARGUMENT.
//
// A quota row is keyed by (silo, kind, class, region), with region
// "global" for a global class, and holds granted, delegated, used and
// consumed (0 in v1). Every amount is an int64 of millionths of the
// class's unit.
//
//   - granted is kept in step with the allocation edges into the silo:
//     from its parent (Grant) or, for the root, from "" (SetCapacity).
//   - delegated is kept in step with the edges out of it.
//   - used is kept in step with the charges it pays (Charge, Refund,
//     Recount). Each charge records payer, home, via and the object it
//     is for. v1 writes payer = home.
//
// The invariant is delegated + used <= granted for every silo, class
// and region, at all times. Only the root can break it, when its
// capacity falls under what the tree holds: it is then over-committed
// in that class and region, and new charges and grants there are
// refused until it is back under (SetCapacity).
//
// # Transactions
//
// Every function runs inside a store.Tx the caller holds, so a charge
// commits together with whatever it pays for, and the fence API can
// create a silo and grant it quota at once. A function that returns an
// error has written nothing: it checks everything before it writes.
package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Global is the region of every row of a global class.
const Global = "global"

// The sorts of class (custom-allocators.md, "Held and metered classes").
const (
	SortHeld    = "held"
	SortMetered = "metered"
)

// The scopes of class.
const (
	ScopeRegional = "regional"
	ScopeGlobal   = "global"
)

// Micro is one whole unit, in the millionths every amount is kept in.
const Micro = 1_000_000

// Unlimited is the capacity of a class the root has no limit in, such
// as memory_mb when install isn't given --root-memory: more than the
// tree can ever hold, with room left so that sums of amounts never
// overflow.
const Unlimited int64 = 1 << 62

// Units returns n whole units as an amount.
func Units(n int64) int64 { return n * Micro }

// FormatAmount writes an amount as a decimal: "16", "0.00025".
func FormatAmount(a int64) string {
	sign := ""
	if a < 0 {
		sign, a = "-", -a
	}
	whole, frac := a/Micro, a%Micro
	if frac == 0 {
		return sign + strconv.FormatInt(whole, 10)
	}
	return sign + strconv.FormatInt(whole, 10) + "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
}

// ParseAmount reads a non-negative decimal with at most six places
// after the point (call A6).
func ParseAmount(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 6 || strings.HasPrefix(whole, "+") || strings.HasPrefix(whole, "-") ||
		strings.HasPrefix(frac, "+") || strings.HasPrefix(frac, "-") {
		return 0, invalid("amount %q: want a non-negative decimal with at most six places", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > (1<<63-1)/Micro-1 {
		return 0, invalid("amount %q: want a non-negative decimal with at most six places", s)
	}
	var f int64
	if frac != "" {
		if f, err = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64); err != nil {
			return 0, invalid("amount %q: want a non-negative decimal with at most six places", s)
		}
	}
	return w*Micro + f, nil
}

// Key names one quota row of a silo.
type Key struct {
	Kind   string
	Class  string
	Region string // Global for a global class
}

func (k Key) String() string { return k.Region + " " + k.Kind + "/" + k.Class }

// Amount is an amount of one class in one region.
type Amount struct {
	Key
	Amount int64 // millionths of the class's unit
}

// Row is one silo's quota row.
type Row struct {
	Silo string
	Key
	Granted       int64
	Delegated     int64
	Used          int64
	Consumed      int64 // metered classes only; 0 in v1
	OverCommitted bool
}

// Free is what the silo can still charge or delegate. It is negative
// in an over-committed root.
func (r Row) Free() int64 { return r.Granted - r.Delegated - r.Used }

// Code is what the fence API turns an Error into.
type Code string

// The codes the ledger returns (docs/silo/fence-api.md, "Errors").
const (
	// QuotaExceeded: not enough free quota in the silo (a charge) or
	// its parent (a grant), or the root is over-committed there.
	QuotaExceeded Code = "QUOTA_EXCEEDED"
	// FailedPrecondition: the change would strand something, such as
	// a grant lowered below what the child holds, or a region removed
	// while still in use.
	FailedPrecondition Code = "FAILED_PRECONDITION"
	// LooserThanParent: a child given a region its parent lacks.
	LooserThanParent Code = "LOOSER_THAN_PARENT"
	// InvalidArgument: an unknown class, a region that doesn't match
	// the class's scope, a negative amount.
	InvalidArgument Code = "INVALID_ARGUMENT"
	// ResourceExhausted: no free aligned slot of the size asked for,
	// or a silo at its block cap (package address).
	ResourceExhausted Code = "RESOURCE_EXHAUSTED"
	// NotFound: no such silo, block or range.
	NotFound Code = "NOT_FOUND"
)

// Error is a refusal from the ledger. errors.Is matches it against
// the sentinel of its code, such as ErrQuotaExceeded.
type Error struct {
	Code    Code
	Message string

	// Where it was refused, when that is one row.
	Silo string
	Key
	Asked int64 // the amount asked for
	Free  int64 // the amount there was
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Is matches a sentinel: an Error with the same code and no message.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Message == "" && t.Code == e.Code
}

// The sentinels, one per Code, for errors.Is.
var (
	ErrQuotaExceeded      = &Error{Code: QuotaExceeded}
	ErrFailedPrecondition = &Error{Code: FailedPrecondition}
	ErrLooserThanParent   = &Error{Code: LooserThanParent}
	ErrInvalidArgument    = &Error{Code: InvalidArgument}
	ErrResourceExhausted  = &Error{Code: ResourceExhausted}
	ErrNotFound           = &Error{Code: NotFound}
)

// CodeOf returns err's Code, or "" if it isn't a ledger Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func invalid(format string, args ...any) *Error {
	return &Error{Code: InvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func precondition(format string, args ...any) *Error {
	return &Error{Code: FailedPrecondition, Message: fmt.Sprintf(format, args...)}
}
