package domain

import "time"

type Printer struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	SystemName     string `json:"system_name"`
	ConnectionType string `json:"connection_type"`
	IPAddress      string `json:"ip_address,omitempty"`
	Port           int    `json:"port"`
	IsActive       bool   `json:"is_active"`
}

type Category struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ColorHex  string `json:"color_hex"`
	SortOrder int    `json:"sort_order"`
	PrinterID *int64 `json:"printer_id,omitempty"`
	IsActive  bool   `json:"is_active"`
}

type Product struct {
	ID          int64   `json:"id"`
	CategoryID  int64   `json:"category_id"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Price       float64 `json:"price"`
	SKU         string  `json:"sku,omitempty"`
	IsAvailable bool    `json:"is_available"`
	SortOrder   int     `json:"sort_order"`
}

type DiningTable struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Zone      string `json:"zone"`
	Status    string `json:"status"` // 'FREE', 'OCCUPIED', 'RESERVED'
	SortOrder int    `json:"sort_order"`
}

type TableSession struct {
	ID         int64      `json:"id"`
	TableID    int64      `json:"table_id"`
	GuestCount int        `json:"guest_count"`
	OpenedAt   time.Time  `json:"opened_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	Status     string     `json:"status"` // 'OPEN', 'CLOSED'
}

type OrderItem struct {
	ID              int64   `json:"id"`
	OrderID         int64   `json:"order_id"`
	BillID          *int64  `json:"bill_id,omitempty"`
	ProductID       int64   `json:"product_id"`
	ProductName     string  `json:"product_name,omitempty"` // Para vistas
	UnitPrice       float64 `json:"unit_price"`
	Quantity        int     `json:"quantity"`
	PrintedQuantity int     `json:"printed_quantity"`
	Notes           string  `json:"notes,omitempty"`
	Status          string  `json:"status"`
}

type Order struct {
	ID        int64       `json:"id"`
	SessionID int64       `json:"session_id"`
	UserID    *int64      `json:"user_id,omitempty"`
	OrderType string      `json:"order_type"`
	Status    string      `json:"status"`
	Notes     string      `json:"notes,omitempty"`
	Items     []OrderItem `json:"items,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}
