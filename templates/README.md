# Templates

Bu klasör kanal bazlı görsel şablon kaynaklarını (logo, arka plan, font vb.)
barındırmak içindir.

MVP'de görsel, `internal/media` paketindeki `TemplateRenderer` tarafından
tamamen koddan (1080x1080 PNG kart) üretilir. Her kategori için arka plan rengi
ve etiket `media.go` içindeki `themes` haritasında tanımlıdır:

- `technology` → TEKNOLOJİ
- `cinema` → SİNEMA
- `news` → HABER
- `economy` → EKONOMİ

İleride bu klasördeki dosyalar (örn. `technology/bg.png`, `logo.png`) okunarak
şablonlar zenginleştirilebilir; `MediaRenderer` arayüzü bunu destekleyecek
şekilde tasarlandı.
