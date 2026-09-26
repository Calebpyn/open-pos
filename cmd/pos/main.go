package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/calebpyn/open-pos/internal/database"
	"github.com/calebpyn/open-pos/internal/handler"
	"github.com/calebpyn/open-pos/web"
)

// version la fija scripts/build-dmg.sh al compilar (-X main.version=...).
var version = "dev"

func main() {
	// El registro también queda en memoria para el diagnóstico descargable.
	log.SetOutput(io.MultiWriter(os.Stderr, handler.LogTail))
	handler.Version = version
	log.Printf("=== Iniciando Open POS Engine %s ===", version)

	// Ctrl+C o el sistema al apagarse: se deja de aceptar peticiones, se
	// terminan las que estén en curso y se cierra la base limpiamente.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go exitWithParent(ctx, cancel)

	// 1. Inicializar SQLite (POS_DB permite ubicar la base en otra carpeta)
	dbPath := os.Getenv("POS_DB")
	if dbPath == "" {
		dbPath = "pos.db"
	}
	db, err := database.InitDB(dbPath)
	if err != nil {
		log.Fatalf("Error fatal al inicializar la base de datos: %v", err)
	}
	defer db.Close()

	// Respaldo diario automático junto a la base (POS_BACKUP_DIR para otra carpeta).
	backupDir := database.BackupDirFor(dbPath)
	go database.ScheduleBackups(ctx, db, backupDir, handler.BackupKeep)
	keepAwake()
	if web.Dev() {
		log.Println("Modo desarrollo: plantillas leídas del disco (POS_WEB_DIR)")
	}

	// Las colas de impresión creadas antes se pausaban ante cualquier falla
	// del USB; se corrigen en segundo plano para no retrasar el arranque.
	go handler.RepairPrinterQueues(context.Background(), db)
	handler.PruneTerminals(ctx, db)

	// 2. Instanciar Handlers
	posHandler := handler.NewPOSHandler(db)
	posHandler.BackupDir = backupDir
	uiHandler := handler.NewUIHandler(posHandler)

	// 3. Registrar Rutas HTTP
	mux := http.NewServeMux()

	// Vistas UI
	mux.HandleFunc("GET /", uiHandler.ServeIndex)
	mux.HandleFunc("GET /api/menu/tree", posHandler.GetMenuTree)
	mux.HandleFunc("GET /api/expo/html", uiHandler.GetExpoHTML)

	// Cobro
	mux.HandleFunc("GET /cobrar/{id}", uiHandler.ServeCheckout)
	mux.HandleFunc("GET /cobrar/mesa/{id}", uiHandler.ServeTableCheckout)
	mux.HandleFunc("GET /tickets/{id}", uiHandler.ServeTicket)

	// Administración (todo lo de aquí requiere sesión de administrador)
	admin := posHandler.RequireAdmin
	mux.HandleFunc("GET /admin/login", uiHandler.ServeAdminLogin)
	mux.HandleFunc("POST /api/admin/login", posHandler.AdminLogin)
	mux.HandleFunc("POST /api/admin/logout", posHandler.AdminLogout)
	mux.HandleFunc("GET /admin", admin(uiHandler.ServeAdminHome))
	mux.HandleFunc("GET /admin/{section}", admin(uiHandler.ServeAdminPage))
	mux.HandleFunc("GET /dispositivos", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/dispositivos", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /api/admin/menu", admin(posHandler.AdminGetMenu))
	mux.HandleFunc("POST /api/admin/categories", admin(posHandler.AdminCreateCategory))
	mux.HandleFunc("PUT /api/admin/categories/{id}", admin(posHandler.AdminUpdateCategory))
	mux.HandleFunc("POST /api/admin/products", admin(posHandler.AdminCreateProduct))
	mux.HandleFunc("PUT /api/admin/products/{id}", admin(posHandler.AdminUpdateProduct))
	mux.HandleFunc("POST /api/admin/products/{id}/availability", admin(posHandler.AdminSetAvailability))
	mux.HandleFunc("POST /api/admin/areas", admin(posHandler.AdminCreateArea))
	mux.HandleFunc("PUT /api/admin/areas/{id}", admin(posHandler.AdminUpdateArea))
	mux.HandleFunc("POST /api/admin/modifier-groups", admin(posHandler.AdminCreateModifierGroup))
	mux.HandleFunc("PUT /api/admin/modifier-groups/{id}", admin(posHandler.AdminUpdateModifierGroup))
	mux.HandleFunc("GET /api/admin/tables", admin(posHandler.AdminListTables))
	mux.HandleFunc("POST /api/admin/tables", admin(posHandler.AdminCreateTable))
	mux.HandleFunc("PUT /api/admin/tables/{id}", admin(posHandler.AdminUpdateTable))
	mux.HandleFunc("PUT /api/admin/tables/{id}/layout", admin(posHandler.AdminMoveTable))
	mux.HandleFunc("GET /api/admin/floor-items", admin(posHandler.AdminListFloorItems))
	mux.HandleFunc("POST /api/admin/floor-items", admin(posHandler.AdminCreateFloorItem))
	mux.HandleFunc("PUT /api/admin/floor-items/{id}", admin(posHandler.AdminUpdateFloorItem))
	mux.HandleFunc("DELETE /api/admin/floor-items/{id}", admin(posHandler.AdminDeleteFloorItem))
	mux.HandleFunc("GET /api/admin/users", admin(posHandler.AdminListUsers))
	mux.HandleFunc("POST /api/admin/users", admin(posHandler.AdminCreateUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", admin(posHandler.AdminUpdateUser))
	mux.HandleFunc("POST /api/admin/zones/rename", admin(posHandler.AdminRenameZone))
	mux.HandleFunc("POST /api/admin/zones/archive", admin(posHandler.AdminArchiveZone))
	mux.HandleFunc("GET /api/admin/reports", admin(posHandler.AdminSalesReport))
	mux.HandleFunc("GET /api/admin/reports/timings", admin(posHandler.AdminTimings))
	mux.HandleFunc("GET /api/admin/history/orders", admin(posHandler.AdminHistoryOrders))
	mux.HandleFunc("GET /api/admin/history/orders/{id}", admin(posHandler.AdminHistoryOrder))
	mux.HandleFunc("GET /api/admin/history/payments", admin(posHandler.AdminHistoryPayments))
	mux.HandleFunc("PUT /api/admin/payments/{id}/payroll", admin(posHandler.AdminReclassifyPayment))
	mux.HandleFunc("GET /api/admin/collaborators", admin(posHandler.AdminListCollaborators))
	mux.HandleFunc("POST /api/admin/collaborators", admin(posHandler.AdminCreateCollaborator))
	mux.HandleFunc("PUT /api/admin/collaborators/{id}", admin(posHandler.AdminUpdateCollaborator))
	mux.HandleFunc("GET /api/admin/payroll", admin(posHandler.AdminPayrollWeek))
	mux.HandleFunc("GET /api/admin/payroll.csv", admin(posHandler.AdminPayrollCSV))
	mux.HandleFunc("POST /api/admin/payroll/settle", admin(posHandler.AdminSettlePayroll))
	mux.HandleFunc("DELETE /api/admin/payroll/settlements/{id}", admin(posHandler.AdminUndoSettlement))
	mux.HandleFunc("GET /api/admin/reports/cuentas.csv", admin(posHandler.AdminBillsCSV))
	mux.HandleFunc("GET /api/admin/print-formats", admin(posHandler.AdminGetPrintFormats))
	mux.HandleFunc("PUT /api/admin/print-formats/receipt", admin(posHandler.AdminSaveReceiptFormat))
	mux.HandleFunc("PUT /api/admin/print-formats/comanda/{id}", admin(posHandler.AdminSaveComandaFormat))
	mux.HandleFunc("POST /api/admin/print-formats/logo", admin(posHandler.AdminUploadLogo))
	mux.HandleFunc("DELETE /api/admin/print-formats/logo", admin(posHandler.AdminDeleteLogo))
	mux.HandleFunc("POST /api/admin/print-formats/preview", admin(posHandler.AdminPreviewFormat))
	mux.HandleFunc("POST /api/admin/print-formats/test", admin(posHandler.AdminTestFormat))
	mux.HandleFunc("GET /api/admin/diagnostics", admin(posHandler.AdminDiagnostics))
	mux.HandleFunc("GET /api/admin/terminals", admin(posHandler.AdminListTerminals))
	mux.HandleFunc("PUT /api/admin/terminals/{id}", admin(posHandler.AdminUpdateTerminal))
	mux.HandleFunc("POST /api/admin/terminals/{id}/logout", admin(posHandler.AdminLogoutTerminal))
	mux.HandleFunc("GET /api/admin/backups", admin(posHandler.AdminListBackups))
	mux.HandleFunc("POST /api/admin/backups", admin(posHandler.AdminCreateBackup))
	mux.HandleFunc("GET /api/admin/backups/{name}", admin(posHandler.AdminDownloadBackup))
	mux.HandleFunc("GET /api/admin/settings", admin(posHandler.AdminGetSettings))
	mux.HandleFunc("PUT /api/admin/settings", admin(posHandler.AdminUpdateSettings))

	// Caja
	mux.HandleFunc("GET /caja", uiHandler.ServeCash)
	mux.HandleFunc("GET /caja/turnos/{id}", uiHandler.ServeCashReport)
	mux.HandleFunc("GET /api/cash/current", posHandler.GetCurrentCash)
	mux.HandleFunc("GET /api/cash/sessions", posHandler.ListCashSessions)
	mux.HandleFunc("POST /api/cash/open", posHandler.OpenCash)
	mux.HandleFunc("POST /api/cash/movements", posHandler.CreateCashMovement)
	mux.HandleFunc("POST /api/cash/drawer", posHandler.OpenDrawer)
	mux.HandleFunc("POST /api/cash/close", posHandler.CloseCash)
	mux.HandleFunc("POST /api/cash/sessions/{id}/notes", posHandler.AddCashNote)
	mux.HandleFunc("POST /api/cash/sessions/{id}/print", posHandler.PrintCashReport)

	// API Endpoints (JSON)
	mux.HandleFunc("GET /api/tables", posHandler.GetTables)
	mux.HandleFunc("GET /api/tables/floor", posHandler.GetFloor)
	mux.HandleFunc("GET /api/terminal/me", posHandler.TerminalMe)
	mux.HandleFunc("POST /api/terminal/login", posHandler.TerminalLogin)
	mux.HandleFunc("POST /api/terminal/logout", posHandler.TerminalLogout)
	mux.HandleFunc("GET /api/app/status", posHandler.AppStatus)
	mux.HandleFunc("GET /api/menu", posHandler.GetMenu)
	mux.HandleFunc("POST /api/orders/add", posHandler.AddToOrder)
	mux.HandleFunc("GET /api/orders/{id}/checkout", posHandler.GetCheckout)
	mux.HandleFunc("POST /api/orders/{id}/pay", posHandler.PayOrder)
	mux.HandleFunc("POST /api/orders/{id}/prebill", posHandler.PrintPreBill)
	mux.HandleFunc("GET /api/collaborators", posHandler.ListCollaborators)
	mux.HandleFunc("POST /api/payments/{id}/voucher", posHandler.PrintPayrollVoucher)
	mux.HandleFunc("POST /api/orders/{id}/cancel", posHandler.CancelOrder)
	mux.HandleFunc("POST /api/orders/{id}/deliver", posHandler.DeliverOrder)
	mux.HandleFunc("POST /api/orders/{id}/reprint", posHandler.ReprintComanda)
	mux.HandleFunc("POST /api/order-items/{id}/void", posHandler.VoidItem)
	mux.HandleFunc("PUT /api/order-items/{id}", posHandler.EditOrderItem)
	mux.HandleFunc("PUT /api/order-items/{id}/guest", posHandler.MoveItemToGuest)
	mux.HandleFunc("GET /api/tables/{id}/guests", posHandler.TableGuests)
	mux.HandleFunc("POST /api/orders/{id}/items", posHandler.AddItemsToOrder)
	mux.HandleFunc("POST /api/order-items/{id}/deliver", posHandler.DeliverItem)
	mux.HandleFunc("POST /api/order-items/{id}/undeliver", posHandler.UndeliverItem)
	mux.HandleFunc("GET /api/customers", posHandler.SearchCustomers)
	mux.HandleFunc("GET /api/printers", admin(posHandler.ListPrinters))
	mux.HandleFunc("GET /api/printers/detect", admin(posHandler.DetectPrinters))
	mux.HandleFunc("POST /api/printers", admin(posHandler.CreatePrinter))
	mux.HandleFunc("PUT /api/printers/{id}", admin(posHandler.UpdatePrinter))
	mux.HandleFunc("DELETE /api/printers/{id}", admin(posHandler.DeletePrinter))
	mux.HandleFunc("POST /api/printers/{id}/test", admin(posHandler.TestPrinter))
	mux.HandleFunc("POST /api/printers/{id}/resume", admin(posHandler.ResumePrinter))
	mux.HandleFunc("GET /api/areas", admin(posHandler.ListAreas))
	mux.HandleFunc("GET /api/menu/modifiers", posHandler.GetMenuModifiers)
	mux.HandleFunc("POST /api/bills/{id}/print", posHandler.PrintBill)

	// Archivos estáticos (van dentro del ejecutable)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	// 4. Iniciar Servidor
	port := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		port = ":" + p
	}
	srv := &http.Server{
		Addr:              port,
		Handler:           handler.TrackTerminals(posHandler.TerminalGuard(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Un ticket con logo puede esperar a la impresora hasta 90 s.
		WriteTimeout: 3 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		log.Println("Apagando: terminando peticiones en curso...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("Servidor corriendo en http://localhost%s", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Error en el servidor HTTP: %v", err)
	}
	log.Println("Servidor detenido")
}
