package opos

import (
	"encoding/json"
	"time"
)

// Event es un registro de la bitácora de Open POS. Seq crece siempre: un
// plugin guarda el último que procesó y pide los siguientes.
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Decode lee el payload en el tipo que corresponde a e.Type.
func (e Event) Decode(v any) error { return json.Unmarshal(e.Payload, v) }

// Tipos de evento (versión 1 de la API). El detalle está en docs/plugins.md.
const (
	EventItemsAdded     = "items.added"     // ItemsAdded: productos nuevos en una orden
	EventItemChanged    = "item.changed"    // ItemChanged: caja editó un producto sin cobrar
	EventItemVoided     = "item.voided"     // ItemVoided: productos anulados
	EventItemSplit      = "item.split"      // ItemSplit: al cobrar parte de un renglón se separa
	EventBillPaid       = "bill.paid"       // BillPaid: cuenta cobrada
	EventOrderClosed    = "order.closed"    // OrderClosed: orden pagada o cancelada
	EventItemsDelivered = "items.delivered" // ItemsDelivered: Expo entregó (o destachó) piezas
	EventCatalogChanged = "catalog.changed" // CatalogChanged: cambió el menú
	EventCashClosed     = "cash.closed"     // CashClosed: corte de turno
)

// Item es un producto de una orden.
type Item struct {
	ItemID    int64      `json:"item_id"`
	OrderID   int64      `json:"order_id"`
	ProductID int64      `json:"product_id"`
	Product   string     `json:"product"`
	Quantity  int        `json:"quantity"`
	UnitPrice float64    `json:"unit_price"` // incluye los extras de modificadores
	Status    string     `json:"status"`     // PENDING, VOID, ...
	Notes     string     `json:"notes"`
	Modifiers []Modifier `json:"modifiers"`
}

// Modifier es una opción elegida ("Leche de almendra"). OptionID es 0 si la
// opción ya no existe en el catálogo.
type Modifier struct {
	OptionID int64  `json:"option_id"`
	Name     string `json:"name"`
}

type ItemsAdded struct {
	OrderID   int64  `json:"order_id"`
	OrderType string `json:"order_type"` // DINE_IN, TAKEAWAY, FLASH
	UserID    *int64 `json:"user_id"`    // nil = compu central
	Items     []Item `json:"items"`
}

type ItemChanged struct {
	OrderID int64 `json:"order_id"`
	ItemID  int64 `json:"item_id"`
	Before  Item  `json:"before"`
	After   Item  `json:"after"`
	UserID  int64 `json:"user_id"` // quién autorizó con su PIN
}

type ItemVoided struct {
	OrderID int64  `json:"order_id"`
	Reason  string `json:"reason"`
	Items   []Item `json:"items"`
}

// ItemSplit: Quantity piezas de ItemID pasaron al renglón nuevo NewItemID;
// en ItemID quedan Remaining.
type ItemSplit struct {
	OrderID   int64 `json:"order_id"`
	ItemID    int64 `json:"item_id"`
	NewItemID int64 `json:"new_item_id"`
	Quantity  int   `json:"quantity"`
	Remaining int   `json:"remaining"`
}

type Payment struct {
	Method string  `json:"method"` // CASH, CARD_DEBIT, CARD_CREDIT, TRANSFER, PAYROLL, OTHER
	Amount float64 `json:"amount"`
}

type BillPaid struct {
	OrderID    int64     `json:"order_id"`
	BillID     int64     `json:"bill_id"`
	BillNumber int       `json:"bill_number"`
	Total      float64   `json:"total"`
	Tip        float64   `json:"tip"`
	Items      []Item    `json:"items"`
	Payments   []Payment `json:"payments"`
	UserID     *int64    `json:"user_id"`
}

// OrderClosed: con Status CANCELLED, UnpaidItems es lo que no se vendió.
type OrderClosed struct {
	OrderID     int64  `json:"order_id"`
	Status      string `json:"status"` // PAID o CANCELLED
	Reason      string `json:"reason"`
	UnpaidItems []Item `json:"unpaid_items"`
}

type DeliveredItem struct {
	ItemID int64 `json:"item_id"`
	Delta  int   `json:"delta"` // piezas entregadas (negativo = se destachó)
}

type ItemsDelivered struct {
	OrderID int64           `json:"order_id"`
	Items   []DeliveredItem `json:"items"`
	UserID  *int64          `json:"user_id"`
}

// CatalogChanged avisa que algo del menú cambió; lo más simple es volver a
// leer el catálogo con Client.Catalog.
type CatalogChanged struct {
	Action  string         `json:"action"` // PRODUCT_UPDATED, PRODUCT_AVAILABILITY, ...
	Details map[string]any `json:"details"`
}

type CashClosed struct {
	CashSessionID int64 `json:"cash_session_id"`
	UserID        int64 `json:"user_id"`
}

// --- Catálogo ---

type Catalog struct {
	Categories     []Category      `json:"categories"`
	Products       []Product       `json:"products"`
	Areas          []Area          `json:"areas"`
	ModifierGroups []ModifierGroup `json:"modifier_groups"`
}

type Category struct {
	ID       int64  `json:"id"`
	ParentID *int64 `json:"parent_id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
}

type Product struct {
	ID          int64   `json:"id"`
	CategoryID  int64   `json:"category_id"`
	AreaID      *int64  `json:"area_id"`
	Name        string  `json:"name"`
	Price       float64 `json:"price"`
	SKU         string  `json:"sku"`
	IsAvailable bool    `json:"is_available"`
	IsActive    bool    `json:"is_active"`
	GroupIDs    []int64 `json:"modifier_group_ids"`
}

type Area struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
}

type ModifierGroup struct {
	ID       int64            `json:"id"`
	Name     string           `json:"name"`
	Min      int              `json:"min"`
	Max      int              `json:"max"`
	IsActive bool             `json:"is_active"`
	Options  []ModifierOption `json:"options"`
}

type ModifierOption struct {
	ID          int64   `json:"id"`
	GroupID     int64   `json:"group_id"`
	Name        string  `json:"name"`
	PriceExtra  float64 `json:"price_extra"`
	IsAvailable bool    `json:"is_available"`
}
