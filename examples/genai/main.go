// Command genai demonstrates tracing a Google Gemini call with neatlogs-go via
// the active WrapGenAI wrapper (the path for apps that call the genai SDK
// directly, without a framework like ADK).
//
// Run:
//
//	export NEATLOGS_API_KEY=...     # spans are dropped if unset
//	export GOOGLE_API_KEY=...       # or GEMINI_API_KEY
//	go run .
//
// Keys may instead be placed in a .env file in this directory.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

func main() {
	loadDotEnv(".env")
	ctx := context.Background()

	// 1. Initialize Neatlogs. This registers the global OpenTelemetry
	//    TracerProvider, so spans from OTel-native frameworks (e.g. Google ADK)
	//    are also captured automatically.
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
		APIKey:       os.Getenv("NEATLOGS_API_KEY"),
		WorkflowName: "genai-example",
		Debug:        true,
	})
	if err != nil {
		log.Fatalf("neatlogs init: %v", err)
	}
	defer shutdown(ctx)

	// 2. Create a Gemini client and wrap it — this is the only added line.
	geminiKey := os.Getenv("GOOGLE_API_KEY")
	if geminiKey == "" {
		geminiKey = os.Getenv("GEMINI_API_KEY")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  geminiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatalf("genai client: %v", err)
	}
	gc := neatlogs.WrapGenAI(client)

	// 3. Call exactly as you would the raw genai client.
	temp := float32(0.7)
	resp, err := gc.GenerateContent(ctx, "gemini-2.5-flash",
		[]*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "In one sentence, what is observability?"}},
		}},
		&genai.GenerateContentConfig{
			Temperature:     &temp,
			MaxOutputTokens: 256,
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: "You are concise."}},
			},
		},
	)
	if err != nil {
		log.Fatalf("generate: %v", err)
	}

	for _, cand := range resp.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				fmt.Println(part.Text)
			}
		}
	}

	if resp.UsageMetadata != nil {
		fmt.Printf("\ntokens: prompt=%d candidates=%d total=%d\n",
			resp.UsageMetadata.PromptTokenCount,
			resp.UsageMetadata.CandidatesTokenCount,
			resp.UsageMetadata.TotalTokenCount,
		)
	}
}

// loadDotEnv reads simple KEY=VALUE lines from path and sets any not already in
// the environment. Missing file is not an error. Keeps the example turnkey
// without a third-party dependency.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
