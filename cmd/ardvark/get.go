package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/jsonout"
)

var (
	getCatalogEntryLimit  int
	getCatalogEntryCursor uint
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Read one stored catalog or entry",
}

var getCatalogCmd = &cobra.Command{
	Use:   "catalog <id|url>",
	Short: "Read a stored catalog by numeric ID or exact source URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runGetCatalog,
}

var getEntryCmd = &cobra.Command{
	Use:   "entry <id|urn>",
	Short: "Read a stored catalog entry by numeric ID or exact URN",
	Args:  cobra.ExactArgs(1),
	RunE:  runGetEntry,
}

func init() {
	getCatalogCmd.Flags().IntVar(&getCatalogEntryLimit, "entry-limit", 50, "maximum catalog entries to return (1-200)")
	getCatalogCmd.Flags().UintVar(&getCatalogEntryCursor, "entry-cursor", 0, "last entry ID from the previous catalog page")
	addJSONFlag(getCatalogCmd)
	addJSONFlag(getEntryCmd)
	getCmd.AddCommand(getCatalogCmd, getEntryCmd)
	rootCmd.AddCommand(getCmd)
}

func runGetCatalog(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	detail, err := jsonout.GetCatalog(cmd.Context(), st, jsonout.GetCatalogOptions{
		Selector:    args[0],
		EntryLimit:  getCatalogEntryLimit,
		EntryCursor: getCatalogEntryCursor,
	})
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(cmd, detail)
	}
	return printCatalogDetail(cmd, detail)
}

func runGetEntry(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	detail, err := jsonout.GetEntry(cmd.Context(), st, args[0])
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(cmd, detail)
	}
	printEntryDetail(cmd, detail)
	return nil
}

func printCatalogDetail(cmd *cobra.Command, detail jsonout.CatalogDetail) error {
	p := printer(cmd)
	p.Header("catalog")
	p.KV("id", detail.ID)
	p.KV("host", detail.Host)
	p.KV("source", detail.SourceURL)
	p.KV("spec version", detail.SpecVersion)
	p.KV("verdict", detail.VerificationStatus)
	p.KV("fetched at", detail.FetchedAt)
	p.KV("entries", len(detail.Entries))
	if detail.NextEntryCursor > 0 {
		p.KV("next entry cursor", detail.NextEntryCursor)
	}
	p.KV("checks", len(detail.Checks))

	if len(detail.Entries) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("get catalog: writing output: %w", err)
	}
	p.Header("entries")
	for _, entry := range detail.Entries {
		name := entry.DisplayName
		if name == "" {
			name = entry.URN
		}
		p.KV(fmt.Sprintf("%d", entry.EntryID), fmt.Sprintf("%s (%s)", name, entry.MediaType))
	}
	return nil
}

func printEntryDetail(cmd *cobra.Command, detail jsonout.EntryDetail) {
	p := printer(cmd)
	p.Header("entry")
	p.KV("id", detail.EntryID)
	p.KV("catalog id", detail.CatalogID)
	p.KV("display name", detail.DisplayName)
	p.KV("urn", detail.URN)
	p.KV("type", detail.MediaType)
	p.KV("host", detail.Host)
	p.KV("catalog", detail.CatalogSourceURL)
	p.KV("verdict", detail.VerificationStatus)
	if detail.RefURL != "" {
		p.KV("url", detail.RefURL)
	}
	p.KV("checks", len(detail.Checks))
}
