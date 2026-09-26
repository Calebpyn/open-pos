-- El cajón de dinero se conecta a una impresora (puerto RJ11). Solo a las
-- que lo tienen se les manda el pulso de apertura: mandárselo a una sin cajón
-- la deja ocupada un momento y el ticket que sigue puede salir cortado.
-- Arranca desactivado; se enciende en Dispositivos al conectar el cajón.
ALTER TABLE printers ADD COLUMN has_drawer BOOLEAN DEFAULT 0;
