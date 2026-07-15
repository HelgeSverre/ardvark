package main

import (
	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/mcpserver"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve ardvark's commands as MCP tools over stdio",
	Long: "mcp runs a stdio MCP (Model Context Protocol) server exposing ardvark's commands as tools: " +
		"ardvark_probe, ardvark_verify, ardvark_crawl, ardvark_seed, ardvark_stats, ardvark_info, and " +
		"ardvark_export. " +
		"Each tool returns the same typed JSON structure as the corresponding command's --json output, " +
		"and uses the same config resolution (--config / ardvark.json in the working directory). " +
		"Protocol messages flow over stdout; diagnostics go to stderr.",
	Args: cobra.NoArgs,
	RunE: runMCP,
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}

func runMCP(cmd *cobra.Command, args []string) error {
	return mcpserver.Run(cmd.Context(), resolvedConfigPath(), version)
}
