// Package web is the contract between the engine and web search and page fetch adapters.
package web

import "context"

// Searcher is a web search engine. A value is built from one settings snapshot and is safe for concurrent use.
type Searcher interface {
	// Search returns at most limit results in the engine's ranking order.
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

// Result is one search hit. A nil field is one the search engine reported as null.
type Result struct {
	Title   *string
	URL     *string
	Content *string
}

// Fetcher reads web pages. A value is built from one settings snapshot and is safe for concurrent use.
type Fetcher interface {
	// Fetch returns the page at url as readable text, read from at most the first 5 MiB of its body. A returned
	// error's text is written for the model.
	Fetch(ctx context.Context, url string) (string, error)
}
