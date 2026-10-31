// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package node

import (
	"errors"
	"fmt"
)

// errNoProcfs is every answer of the host of a platform without Linux
// procfs.
var errNoProcfs = fmt.Errorf("no Linux /proc on this platform: %w", errors.ErrUnsupported)

// newHost returns the host of a platform without Linux procfs, whose every
// call fails with errors.ErrUnsupported. ruralz node drain is not planned
// here, and its Spec.Platform refuses before the command runs.
func newHost() host { return noProcfs{} }

// noProcfs is the host of a platform without Linux procfs.
type noProcfs struct{}

func (noProcfs) pidNamespace() (string, error) { return "", errNoProcfs }

func (noProcfs) procStat(int) ([]byte, error) { return nil, errNoProcfs }

func (noProcfs) locks() ([]byte, error) { return nil, errNoProcfs }

func (noProcfs) inode(string) (uint64, error) { return 0, errNoProcfs }

func (noProcfs) open(int) (process, error) { return nil, errNoProcfs }
