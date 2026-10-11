// Package silo is the root of Silo, a system of nested virtual clouds.
//
// A silo is a node in a tree. Inside one, a tenant gets a GCE-compatible
// compute/v1 API (the facade), fenced so that everything it makes stays
// inside its silo. The control plane, silod, keeps the tree, the
// allocators, the grants and the audit log in a database reached only
// through package store.
//
// The normative specification is docs/silo/*.md in this repository;
// docs/silo/README.md is the overview and wins on any conflict.
// Requirement headings there carry stable ids, {#SV-n}, which code and
// commits cite.
//
// Silo was developed inside github.com/bwsalmon/grain and moved to this
// repository, github.com/bwsalmon/silo, with its layout unchanged: the
// code is under pkg/silo and cmd/silo*, and the specification under
// docs/silo, so paths cited in commits and test reports still resolve.
package silo
