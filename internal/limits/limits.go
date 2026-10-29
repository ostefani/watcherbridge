// Package limits holds the sizing rails shared by the store and config
// layers. They live here — not in internal/store — so operator-facing
// validation in internal/config can check them without importing the
// sqlite driver (and its cgo toolchain) into config tests, and without
// a future import cycle when the store consumes config policy.
package limits

// MaxGroupBytes caps the raw payload bytes a group may accumulate.
// TakeGroup reads every payload inside the write transaction and the
// worker then decodes, tokenizes and re-marshals them (roughly 4x
// transient), so the cap bounds both writer-hold time and heap. A
// joining event whose bytes would cross the cap closes the old group and
// founds the next one in the same persist transaction, so the bound is
// strict: no group ever holds more. When a group is full the crossing
// event founds the next group, exactly like the event-count cap —
// subgroups still get briefed, nothing is dropped. 8 MiB bounds the
// TakeGroup read against the ingest persist budget. Enforced in
// PersistEventAndGroup; validated against MaxBodyBytes at startup so a
// single event always fits a fresh group.
const MaxGroupBytes = 8 << 20
