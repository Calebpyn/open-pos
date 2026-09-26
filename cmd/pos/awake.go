package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// keepAwake evita que la Mac se duerma por inactividad mientras el servidor
// corre: dormida, el iPad y las demás terminales se quedan sin POS. caffeinate
// suelta el candado solo cuando este proceso termina (-w). La pantalla sí se
// puede apagar.
func keepAwake() {
	if runtime.GOOS != "darwin" {
		return
	}
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		log.Printf("No se pudo evitar la suspensión: %v", err)
		return
	}
	go cmd.Wait()
}
