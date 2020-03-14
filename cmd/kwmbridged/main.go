/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package main

import (
	"fmt"
	"os"

	"stash.kopano.io/kwm/kwmbridge/cmd"
)

func main() {
	cmd.RootCmd.AddCommand(commandServe())
	cmd.RootCmd.AddCommand(commandHealthcheck())

	if err := cmd.RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
