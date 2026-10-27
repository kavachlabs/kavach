package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/segmentio/kafka-go"
)

func main() {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{os.Getenv("KAFKA_BROKER")},
		Topic:   "payments",
		GroupID: "payments-consumer",
	})
	defer r.Close()

	store := NewStore()
	for {
		m, err := r.FetchMessage(context.Background())
		if err != nil {
			log.Fatalf("fetch: %v", err)
		}
		var ev Event
		if err := json.Unmarshal(m.Value, &ev); err != nil {
			log.Printf("skip malformed message at offset %d: %v", m.Offset, err)
		} else if err := store.Apply(ev); err != nil {
			log.Printf("apply %s: %v", ev.ID, err)
		}
		if err := r.CommitMessages(context.Background(), m); err != nil {
			log.Fatalf("commit: %v", err)
		}
	}
}
