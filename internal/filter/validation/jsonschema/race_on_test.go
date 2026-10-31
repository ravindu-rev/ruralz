// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package jsonschema

// raceEnabled reports a -race build: allocation counts are not compared,
// since sync.Pool then drops items at random, and the tests of large bodies
// use a tenth or a quarter of them, since the detector slows the library
// about tenfold.
const raceEnabled = true
