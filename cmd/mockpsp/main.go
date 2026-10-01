package main

import (
	"log"
	"net/http"
	"time"

	"invoicesvc/internal/mockpsp"
)

func main() {
	log.Fatal(http.ListenAndServe(":8081", mockpsp.New(100*time.Millisecond, 30*time.Second)))
}
