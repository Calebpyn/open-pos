package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"strings"
	"time"
)

// Dónde puede estar el comando de Tailscale en la Mac (app de la App Store o
// descarga directa) o en Linux.
var tailscaleCLIs = []string{
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
	"tailscale",
}

type remoteStatus struct {
	Installed bool     `json:"installed"`
	Connected bool     `json:"connected"`
	State     string   `json:"state,omitempty"`
	DNSName   string   `json:"dns_name,omitempty"`
	IPs       []string `json:"ips"`
	URLs      []string `json:"urls"`
	Port      string   `json:"port"`
	ViaRemote bool     `json:"via_remote"`
}

// tailscaleIPs busca en las interfaces de red las direcciones de Tailscale.
// No necesita el comando: basta con que Tailscale esté conectado.
func tailscaleIPs() []string {
	out := []string{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		for _, n := range tailscaleNets {
			if n.Contains(ipn.IP) {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// tailscaleCLIStatus pregunta al comando de Tailscale el estado y el nombre
// MagicDNS de esta computadora. Si no hay comando, installed = false.
func tailscaleCLIStatus(ctx context.Context) (installed bool, state, dnsName string) {
	for _, bin := range tailscaleCLIs {
		path, err := osexec.LookPath(bin)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		out, err := osexec.CommandContext(ctx, path, "status", "--json").Output()
		cancel()
		if err != nil && len(out) == 0 {
			return true, "", ""
		}
		var st struct {
			BackendState string
			Self         struct{ DNSName string }
		}
		if json.Unmarshal(out, &st) == nil {
			return true, st.BackendState, strings.TrimSuffix(st.Self.DNSName, ".")
		}
		return true, "", ""
	}
	return false, "", ""
}

// GET /api/admin/remote - Estado de Tailscale y direcciones para entrar de fuera.
func (h *POSHandler) AdminRemoteStatus(w http.ResponseWriter, r *http.Request) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	st := remoteStatus{Port: port, IPs: tailscaleIPs(), URLs: []string{}, ViaRemote: isTailscale(r)}
	st.Installed, st.State, st.DNSName = tailscaleCLIStatus(r.Context())
	if len(st.IPs) > 0 {
		// Si hay dirección de Tailscale, Tailscale está instalado aunque no
		// encontremos el comando.
		st.Installed = true
		st.Connected = st.State == "" || st.State == "Running"
	}
	if st.Connected {
		if st.DNSName != "" {
			st.URLs = append(st.URLs, "http://"+st.DNSName+":"+port+"/admin")
		}
		for _, ip := range st.IPs {
			if strings.Contains(ip, ":") {
				continue // la IPv6 es difícil de teclear; con la IPv4 basta
			}
			st.URLs = append(st.URLs, "http://"+ip+":"+port+"/admin")
		}
	}
	writeJSON(w, http.StatusOK, st)
}
