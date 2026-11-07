// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"strings"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// capability is one Plugin ABI v1 Capability as a bit of a capSet.
type capability uint8

// The Capabilities, in the order of pkg/config/v1alpha1
// (docs/architecture/05-wasm-plugin-system.md "Capabilities and
// sandboxing").
const (
	capCredentialsRead capability = iota
	capRequestBodyRead
	capRequestHeadersRead
	capResponseBodyRead
	capStreamRead
	capResponseSend
	capStateRead
	capStateWrite
	capRequestHeadersWrite
	capRequestBodyWrite
	capConsumerRead
	capResponseHeadersWrite
	capResponseBodyWrite
	capStreamWrite
	capLogWrite
	capMetricsWrite
	capClockRead
	capRandomRead
	capRequestMetadataRead
	capResponseHeadersRead
	capContextUse
	capCryptoUse
	numCapabilities
	// capNone marks a WASI import that needs no Capability.
	capNone capability = 0xff
)

// capabilityNames returns the configuration spelling of each Capability,
// indexed by capability.
func capabilityNames() [numCapabilities]v1alpha1.Capability {
	return [numCapabilities]v1alpha1.Capability{
		capCredentialsRead:      v1alpha1.CapabilityCredentialsRead,
		capRequestBodyRead:      v1alpha1.CapabilityRequestBodyRead,
		capRequestHeadersRead:   v1alpha1.CapabilityRequestHeadersRead,
		capResponseBodyRead:     v1alpha1.CapabilityResponseBodyRead,
		capStreamRead:           v1alpha1.CapabilityStreamRead,
		capResponseSend:         v1alpha1.CapabilityResponseSend,
		capStateRead:            v1alpha1.CapabilityStateRead,
		capStateWrite:           v1alpha1.CapabilityStateWrite,
		capRequestHeadersWrite:  v1alpha1.CapabilityRequestHeadersWrite,
		capRequestBodyWrite:     v1alpha1.CapabilityRequestBodyWrite,
		capConsumerRead:         v1alpha1.CapabilityConsumerRead,
		capResponseHeadersWrite: v1alpha1.CapabilityResponseHeadersWrite,
		capResponseBodyWrite:    v1alpha1.CapabilityResponseBodyWrite,
		capStreamWrite:          v1alpha1.CapabilityStreamWrite,
		capLogWrite:             v1alpha1.CapabilityLogWrite,
		capMetricsWrite:         v1alpha1.CapabilityMetricsWrite,
		capClockRead:            v1alpha1.CapabilityClockRead,
		capRandomRead:           v1alpha1.CapabilityRandomRead,
		capRequestMetadataRead:  v1alpha1.CapabilityRequestMetadataRead,
		capResponseHeadersRead:  v1alpha1.CapabilityResponseHeadersRead,
		capContextUse:           v1alpha1.CapabilityContextUse,
		capCryptoUse:            v1alpha1.CapabilityCryptoUse,
	}
}

// String returns the configuration spelling.
func (c capability) String() string {
	if c >= numCapabilities {
		return ""
	}
	return string(capabilityNames()[c])
}

// capSet is a set of Capabilities.
type capSet uint32

// has reports whether c is in s.
func (s capSet) has(c capability) bool { return c < numCapabilities && s&(1<<c) != 0 }

// add returns s with c.
func (s capSet) add(c capability) capSet {
	if c >= numCapabilities {
		return s
	}
	return s | 1<<c
}

// minus returns the Capabilities of s that t lacks.
func (s capSet) minus(t capSet) capSet { return s &^ t }

// String lists s in Capability order, comma-separated.
func (s capSet) String() string {
	var b strings.Builder
	for c := capability(0); c < numCapabilities; c++ {
		if !s.has(c) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.String())
	}
	return b.String()
}

// parseCapabilities returns the set of a Plugin's spec.capabilities; the
// schema's enum admits only known identifiers, so an unknown one, which
// grants nothing, is ignored.
func parseCapabilities(cs []v1alpha1.Capability) capSet {
	names := capabilityNames()
	var s capSet
	for _, c := range cs {
		for i, n := range names {
			if n == c {
				s = s.add(capability(i))
				break
			}
		}
	}
	return s
}
