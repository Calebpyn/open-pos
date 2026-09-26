-- Datos para administrar impresoras desde la sección de dispositivos.
-- connection_type: 'CUPS' (USB, vía una cola de CUPS llamada system_name)
--                  'NETWORK' (ESC/POS directo por TCP a ip_address:port)
ALTER TABLE printers ADD COLUMN device_uri TEXT;          -- URI de CUPS del dispositivo (usb://...)
ALTER TABLE printers ADD COLUMN paper_width INTEGER DEFAULT 80; -- ancho de papel en mm (58 u 80)
ALTER TABLE printers ADD COLUMN created_at DATETIME;
