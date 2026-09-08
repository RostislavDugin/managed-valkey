// Отдельная команда нужна HEALTHCHECK: в runtime-образе нет shell и curl.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

const requestTimeout = 3 * time.Second

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	if err := check(ctx, client, args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}

func check(ctx context.Context, client *http.Client, endpoint string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}

	if err := response.Body.Close(); err != nil {
		return fmt.Errorf("close response: %w", err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected status: %s", response.Status)
	}

	return nil
}
