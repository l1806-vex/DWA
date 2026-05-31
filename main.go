package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"

	"dwa/internal/client"
	"dwa/internal/pow"
	"dwa/internal/server"
)

func main() {
	// start.bat always cds to the project root before running the binary,
	// so .env and sha3_pow.wasm are reliably in the working directory.
	godotenv.Load(".env") //nolint:errcheck

	host := flag.String("host", getenv("HOST", "127.0.0.1"), "bind host")
	port := flag.Int("port", getenvInt("PORT", 8000), "bind port")
	flag.Parse()

	token := os.Getenv("DEEPSEEK_TOKEN")
	if token == "" {
		log.Fatal("DEEPSEEK_TOKEN is not set. Add it to .env or export it.")
	}

	solver, err := pow.New("sha3_pow.wasm")
	if err != nil {
		log.Fatalf("POW init: %v", err)
	}

	ds := client.New(token, solver)
	srv := server.New(ds)

	addr := fmt.Sprintf("%s:%d", *host, *port)
	fmt.Printf("\n DWA — DeepSeek Web API\n")
	fmt.Printf(" OpenAI:    http://%s/v1/chat/completions\n", addr)
	fmt.Printf(" Anthropic: http://%s/v1/messages\n", addr)
	fmt.Printf(" Models:    http://%s/v1/models\n\n", addr)

	log.Fatal(srv.ListenAndServe(addr))
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
