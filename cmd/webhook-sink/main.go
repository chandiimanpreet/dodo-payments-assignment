// Demo-only receiver: logs headers and body of every request and returns 200.
// It does no signature verification (see the README for how a receiver verifies).
package main

import (
	"io"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("webhook received: Webhook-Id=%s Webhook-Timestamp=%s Webhook-Signature=%s body=%s",
			r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp"), r.Header.Get("Webhook-Signature"), body)
		w.WriteHeader(http.StatusOK)
	})
	log.Fatal(http.ListenAndServe(":9000", nil))
}
