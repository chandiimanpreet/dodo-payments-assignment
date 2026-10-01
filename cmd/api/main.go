package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"invoicesvc/internal/app"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := app.Migrate(dbURL); err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	a := app.New(db, log, os.Getenv("PSP_URL"), 5*time.Second, 40*time.Second)
	if err := a.SeedIfEmpty(ctx); err != nil {
		return err
	}
	log.Info("listening", "addr", ":8080")
	return http.ListenAndServe(":8080", a.Routes())
}
