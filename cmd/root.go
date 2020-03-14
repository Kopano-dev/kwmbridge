/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

// RootCmd provides the commandline parser root.
var RootCmd = &cobra.Command{
	Use: "kwmbridged",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
		os.Exit(2)
	},
}
