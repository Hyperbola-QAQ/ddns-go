//go:build !linux

package config

// Standard net.Interfaces remains the portable fallback when lifetimes are unavailable.
func ipv6Lifetimes() map[string]AddressLifetime { return nil }
