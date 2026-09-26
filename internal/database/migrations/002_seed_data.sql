-- Insertar Impresoras de prueba
INSERT INTO printers (id, name, system_name) VALUES 
(1, 'Barra', 'EPSON_TM_BARRA'),
(2, 'Cocina', 'EPSON_TM_COCINA');

-- Insertar Categorías
INSERT INTO categories (id, name, color_hex, sort_order, printer_id) VALUES 
(1, 'Cafetería', '#D97706', 1, 1),
(2, 'Cocina Caliente', '#DC2626', 2, 2),
(3, 'Postres', '#10B981', 3, 1);

-- Insertar Productos
INSERT INTO products (category_id, name, price, description) VALUES 
(1, 'Espresso Doble', 45.00, 'Café concentrado de grano de especialidad'),
(1, 'Latte 12oz', 65.00, 'Espresso con leche cremada'),
(2, 'Chilaquiles Verdes', 120.00, 'Con totopos de maíz, crema y queso fresco'),
(3, 'Cheesecake de Frutos Rojos', 85.00, 'Rebanada individual');

-- Insertar Mesas
INSERT INTO dining_tables (name, zone, status, sort_order) VALUES 
('Mesa 1', 'Interior', 'FREE', 1),
('Mesa 2', 'Interior', 'FREE', 2),
('Mesa 3', 'Terraza', 'FREE', 3),
('Barra 1', 'Barra', 'FREE', 4);
