-- Por qué se compra (o no) en cada lugar: "conviene por caja", "solo si no
-- hay en Costco". La ve quien surte junto a las alternativas.
ALTER TABLE compras_sources ADD COLUMN note TEXT NOT NULL DEFAULT '';
