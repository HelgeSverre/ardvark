package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/jsonout"
)

var (
	searchHost      string
	searchPublisher string
	searchMediaType string
	searchTag       string
	searchVerdict   string
	searchLimit     int
	searchCursor    uint
)

var searchCmd = &cobra.Command{
	Use:   "search [text]",
	Short: "Search indexed catalog entries",
	Long: "search finds catalog entries by free text and exact metadata filters. " +
		"Text matching is case-insensitive across URNs, names, descriptions, tags, representative " +
		"queries, and reference URLs. Results use an entry-ID cursor for stable pagination.",
	Args: cobra.MaximumNArgs(1),
	RunE: runSearch,
}

func init() {
	searchCmd.Flags().StringVar(&searchHost, "host", "", "exact serving host")
	searchCmd.Flags().StringVar(&searchPublisher, "publisher", "", "exact URN publisher")
	searchCmd.Flags().StringVar(&searchMediaType, "type", "", "exact entry media type")
	searchCmd.Flags().StringVar(&searchTag, "tag", "", "exact entry tag")
	searchCmd.Flags().StringVar(&searchVerdict, "verdict", "", "catalog verdict: valid, valid_with_warnings, or invalid")
	searchCmd.Flags().IntVar(&searchLimit, "limit", 50, "maximum results to return (1-200)")
	searchCmd.Flags().UintVar(&searchCursor, "cursor", 0, "last entry ID from the previous page")
	addJSONFlag(searchCmd)
	rootCmd.AddCommand(searchCmd)
}

func runSearch(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	query := ""
	if len(args) == 1 {
		query = args[0]
	}
	res, err := jsonout.Search(cmd.Context(), st, jsonout.SearchOptions{
		Query:     query,
		Host:      searchHost,
		Publisher: searchPublisher,
		MediaType: searchMediaType,
		Tag:       searchTag,
		Verdict:   searchVerdict,
		Limit:     searchLimit,
		Cursor:    searchCursor,
	})
	if err != nil {
		return err
	}

	if jsonOut {
		return printJSON(cmd, res)
	}
	return printSearchResult(cmd, res)
}

func printSearchResult(cmd *cobra.Command, res jsonout.SearchResult) error {
	p := printer(cmd)
	if len(res.Entries) == 0 {
		p.Mutedf("no entries found")
		return nil
	}

	for i, entry := range res.Entries {
		if i > 0 {
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("search: writing output: %w", err)
			}
		}
		title := entry.DisplayName
		if title == "" {
			title = entry.URN
		}
		if title == "" {
			title = fmt.Sprintf("entry %d", entry.EntryID)
		}
		p.Header(title)
		p.KV("entry id", entry.EntryID)
		p.KV("urn", entry.URN)
		p.KV("type", entry.MediaType)
		p.KV("host", entry.Host)
		p.KV("verdict", entry.VerificationStatus)
		if entry.RefURL != "" {
			p.KV("url", entry.RefURL)
		}
	}

	parts := []string{fmt.Sprintf("%d entries", len(res.Entries))}
	if res.NextCursor > 0 {
		parts = append(parts, fmt.Sprintf("next cursor %d", res.NextCursor))
	}
	p.Summary("search complete", parts...)
	return nil
}
