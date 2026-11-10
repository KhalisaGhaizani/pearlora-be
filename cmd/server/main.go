// Command server is the Pearlora control backend.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"pearlora/backend/internal/api"
	"pearlora/backend/internal/app"
	"pearlora/backend/internal/device"
	"pearlora/backend/internal/settings"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	settingsPath := flag.String("settings", "settings.json", "settings file (created on first save)")
	flag.Parse()

	s, err := settings.Load(*settingsPath)
	if err != nil {
		log.Fatal(err)
	}

	engine := app.NewEngine(s, *settingsPath)
	ui := api.NewUIHub(engine.Status)
	dev := device.NewHub(engine)
	engine.Attach(dev, ui)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)

	srv := &http.Server{Addr: *addr, Handler: api.Routes(engine, ui, dev)}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("Pearlora backend on %s (mode: manual; device at /ws/device, frontend at /ws)", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
