package main

import (
	"context"
	"log"
	"os"
	"time"
)

// exitWithParent apaga el servidor si la app de Mac que lo abrió deja de
// existir (p. ej. se cerró a la fuerza): sin esto quedaría corriendo huérfano,
// ocupando el puerto. Solo aplica cuando la app lo pide con OPEN_POS_APP=1.
func exitWithParent(ctx context.Context, cancel context.CancelFunc) {
	if os.Getenv("OPEN_POS_APP") != "1" {
		return
	}
	parent := os.Getppid()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if os.Getppid() != parent {
				log.Println("La app de Open POS se cerró; apagando el servidor")
				cancel()
				return
			}
		}
	}
}
