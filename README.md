# ai-social-publisher

AI destekli, **onaylı** Instagram içerik yayınlama sistemi. Haberleri çeker, AI
ile skorlar, önemli olanları Telegram üzerinden kullanıcıya bildirir, kullanıcı
onayıyla dinamik sayıda post alternatifi üretir, seçilen alternatif için görsel
hazırlar ve Instagram Graph API ile yayınlar.

> **Mimari:** Mikroservis değil. Tek bir Go uygulaması (modüler monolith).
> İçeride temiz paket yapısı vardır; dışarıda yalnızca **haber-servisi**,
> **telegram-servisi**, **Instagram Graph API** ve **AI sağlayıcılar** (tgpt /
> Ollama) bulunur.

---

## İçindekiler

- [Proje amacı](#proje-amacı)
- [Mimari](#mimari)
- [Harici servisler](#harici-servisler)
- [AI provider chain mantığı](#ai-provider-chain-mantığı)
- [tgpt kurulumu](#tgpt-kurulumu)
- [Ollama fallback kurulumu](#ollama-fallback-kurulumu)
- [Kurulum ve lokal çalıştırma](#kurulum-ve-lokal-çalıştırma)
- [PostgreSQL başlatma](#postgresql-başlatma)
- [Config örneği](#config-örneği)
- [Instagram Graph API ayarları](#instagram-graph-api-ayarları)
- [Telegram callback akışı](#telegram-callback-akışı)
- [Yönetim paneli](#yönetim-paneli)
- [Docker Compose kullanımı](#docker-compose-kullanımı)
- [HTTP API ve örnek curl komutları](#http-api-ve-örnek-curl-komutları)
- [Status flow](#status-flow)
- [Görsel üretimi](#görsel-üretimi)
- [Klasör yapısı](#klasör-yapısı)
- [Test](#test)

---

## Proje amacı

4 farklı Instagram kanalı için içerik üretmek:

| Kanal      | Kategori     | Varsayılan alternatif | Bildirim eşiği |
|------------|--------------|-----------------------|----------------|
| teknoloji  | `technology` | 5                     | 80             |
| sinema     | `cinema`     | 3                     | 75             |
| haber      | `news`       | 3                     | 85             |
| ekonomi    | `economy`    | 5                     | 80             |

Akış:

1. Belirli aralıklarla `haber-servisi`'nden haber çekilir.
2. Duplicate kontrolü yapılır (`external_news_id` UNIQUE + job seviyesinde tekillik).
3. Haber AI ile skorlanır (önem + virallik + risk).
4. Skor eşik üzerindeyse `telegram-servisi` ile kullanıcıya bildirim gider.
5. Kullanıcı **"Post Hazırla"** derse config'teki `variant_count` kadar alternatif üretilir.
6. Üretilen her alternatif için Telegram'a **dinamik** sayıda buton gönderilir.
7. Kullanıcı bir alternatif seçince görsel hazırlanır, public URL elde edilir.
8. İçerik Instagram Graph / Content Publishing API ile yayınlanır.
9. Sonuç DB'ye (`publish_logs`) kaydedilir ve kullanıcıya bildirilir.

---

## Mimari

```
                ┌────────────────────────── ai-social-publisher (tek uygulama) ──────────────────────────┐
                │                                                                                          │
 haber-servisi ─┼─▶ news.Client ─▶ news.Repository ─▶ approval.Service ─┬─▶ ai.Service (tgpt→ollama)       │
                │                                                       ├─▶ media.Renderer ─▶ storage      │
 telegram-svc ◀─┼── telegram.Client ◀───────────────────────────────────┤                                  │
       │        │                                                       └─▶ instagram.Publisher ──────────┼─▶ Instagram Graph API
       └────────┼─▶ POST /api/telegram/callback ─▶ approval.Service                                        │
                │                                                                                          │
                │   scheduler (queues · retry · outbox · publish)                 PostgreSQL               │
                └──────────────────────────────────────────────────────────────────────────────────────────┘
```

Her sorumluluk `internal/` altında ayrı bir pakettir; paketler arası bağımlılık
tek yönlüdür ve `approval` paketi orkestrasyonu yapar.

---

## Harici servisler

| Servis              | Rolü                                                              |
|---------------------|-------------------------------------------------------------------|
| **haber-servisi**   | Haber kaynağı. `GET /api/news?category=...`                       |
| **telegram-servisi**| Bildirim gönderir (`POST /api/notifications`) ve callback iletir. |
| **Instagram Graph** | İçeriği bu uygulama yayınlar. Telegram'ın yayınla yetkisi yoktur. |
| **AI sağlayıcılar** | `tgpt` (CLI) primary, **Ollama** (HTTP) fallback.                 |

`haber-servisi` ve `telegram-servisi` çağrıları ayrı Bearer token'larla kimlik
doğrular (`NEWS_SERVICE_AUTH_TOKEN`, `TELEGRAM_SERVICE_AUTH_TOKEN`).

---

## AI provider chain mantığı

Sıra **kesinlikle** şöyledir:

1. **Primary:** `tgpt` CLI
2. **Fallback:** Ollama HTTP API
3. İkisi de başarısız olursa ilgili job **`WAITING_AI`** durumuna alınır;
   exponential backoff ile en fazla 8 kez denenir, sonra `FAILED` olur.

```go
// internal/ai/service.go
aiSvc := ai.NewService(logger,
    ai.NewTgptProvider(cfg.AI.Providers.Tgpt, logger),   // 1. primary
    ai.NewOllamaProvider(cfg.AI.Providers.Ollama, logger), // 2. fallback
)
```

Her provider `AIProvider` arayüzünü uygular:

```go
type AIProvider interface {
    Name() string
    IsAvailable(ctx context.Context) bool
    ScoreNews(ctx context.Context, news NewsCandidate) (*NewsScore, error)
    GeneratePostVariants(ctx context.Context, req GeneratePostVariantsRequest) ([]PostVariant, error)
}
```

**Davranış:**
- tgpt sistemde yoksa (`PATH`'te bulunamazsa) veya config'te `enabled: false` ise
  `IsAvailable` `false` döner ve atlanır.
- tgpt timeout / hata / boş çıktı / geçersiz JSON dönerse Ollama denenir.
- Markdown kod bloğu (` ```json `) gelirse temizlenir (`internal/ai/parse.go`).
- AI çıktısı her zaman JSON beklenir; skorlar `0-100` aralığına clamp edilir.
- Enum değerleri, caption uzunluğu ve istenen alternatif sayısı doğrulanır.
- Haber metni güvenilmeyen veri olarak ayrılır; metin içindeki talimatlar uygulanmaz.
- **Token / hassas veri loglanmaz.**

---

## tgpt kurulumu

`tgpt` bir komut satırı LLM aracıdır. Kurulum:

```bash
# Önerilen (script):
curl -sSL https://raw.githubusercontent.com/aandrew-me/tgpt/main/install | bash -s /usr/local/bin

# veya Go ile:
go install github.com/aandrew-me/tgpt/v2@latest
```

Doğrulama:

```bash
tgpt "merhaba"
```

Config'ten komut değiştirilebilir:

```yaml
ai:
  providers:
    tgpt:
      enabled: true
      command: "tgpt"       # PATH'te değilse tam yol verin
      timeout_seconds: 120
```

> tgpt kurulu değilse uygulama çökmez; otomatik olarak Ollama'ya düşer.

---

## Ollama fallback kurulumu

```bash
# Kurulum (Linux/macOS):
curl -fsSL https://ollama.com/install.sh | sh

# Model indir ve servisi başlat:
ollama pull llama3.1:8b
ollama serve   # http://localhost:11434
```

Sağlık kontrolü `GET /api/tags` ile yapılır; üretim `POST /api/generate`
(non-streaming, `format: json`) ile.

```yaml
ai:
  providers:
    ollama:
      enabled: true
      base_url: "http://localhost:11434"
      model: "llama3.1:8b"
      timeout_seconds: 90
```

---

## Kurulum ve lokal çalıştırma

Gereksinimler: **Go 1.26+**, **PostgreSQL 14+**. (tgpt/Ollama opsiyonel — yoksa
joblar `WAITING_AI`'da bekler.)

```bash
# 1) bağımlılıklar
go mod download

# 2) env ve config hazırla
cp .env.example .env
cp config.example.yaml config.yaml   # gerekirse düzenleyin

# 3) development PostgreSQL'i başlat
make db-up

# 4) development ortamında çalıştır (auto_migrate=true ise migration otomatik uygulanır)
# Uygulama .env dosyasını başlangıçta otomatik yükler.
make run dev

# farklı config dosyasıyla çalıştırmak için
make run dev CONFIG=config.example.yaml

# veya
APP_ENV=development go run ./cmd/server serve --config config.yaml
```

Migration'ı elle uygulamak için:

```bash
APP_ENV=development go run ./cmd/server migrate up      # tüm migration'ları uygula
APP_ENV=development go run ./cmd/server migrate down    # son migration'ı geri al
```

---

## PostgreSQL başlatma

Sadece veritabanını Docker ile ayağa kaldırmak için:

```bash
make db-up
```

DSN örneği (`.env` içindeki `DATABASE_URL`):

```
postgres://aisp:aisp_secret@localhost:5433/ai_social_publisher?sslmode=disable
```

Varsayılan development portu `5433`'tür. Böylece makinede 5432 kullanan başka
PostgreSQL servisleriyle çakışmaz. Farklı port gerekiyorsa `.env` içinde
`POSTGRES_PORT` ve `DATABASE_URL` değerlerini birlikte değiştirin.

`password authentication failed for user "aisp"` hatası alırsanız hedef portta
eski parola ile oluşturulmuş bir PostgreSQL çalışıyordur. Bu durumda mevcut DB
parolasını `.env` içindeki `DATABASE_URL` ile eşitleyin veya development
veritabanı volume'unu sıfırlayıp `make db-up` ile yeniden oluşturun.

Migration'lar `migrations/*.sql` içinde **goose** formatındadır ve uygulamaya
gömülüdür (`embed.FS`); `database.auto_migrate: true` ile açılışta uygulanır.

---

## Config örneği

Tüm seçenekler için `config.example.yaml`. Önemli bloklar:

```yaml
post_generation:
  default_variant_count: 3   # hesapta variant_count yoksa kullanılır
  max_variant_count: 10      # üstündeki değerler buna düşürülür

accounts:
  - code: "teknoloji"
    category: "technology"
    instagram_user_id: "${IG_TECH_USER_ID}"
    variant_count: 5         # dinamik alternatif sayısı (sabit değil!)
    notify_threshold: 80
    styles: ["Kısa ve vurucu", "Haber dili", ...]
```

`${VAR}` placeholder'ları yükleme anında ortam değişkenlerinden genişletilir.
Hesaplar açılışta `social_accounts` tablosuna upsert edilir (config = doğruluk
kaynağı).

Config strict olarak ayrıştırılır: bilinmeyen YAML alanları, eksik environment
variable'ları ve geçersiz URL/eşik değerleri uygulamayı başlatmaz.

Yönetim API'si ve Telegram callback güvenliği:

```yaml
security:
  api_token: "${API_AUTH_TOKEN}"                       # minimum 32 karakter
  telegram_callback_secret: "${TELEGRAM_CALLBACK_SECRET}"
  allowed_telegram_users: ["${TELEGRAM_ALLOWED_USER}"]
```

---

## Instagram Graph API ayarları

```yaml
instagram:
  graph_base_url: "https://graph.facebook.com"
  api_version: "v23.0"
  access_token: "${INSTAGRAM_ACCESS_TOKEN}"
  publish_enabled: false     # false → DRY-RUN (gerçek istek atılmaz)
```

Yayınlama iki adımlıdır:

```
POST /{ig_user_id}/media           (image_url + caption) → creation_id
POST /{ig_user_id}/media_publish   (creation_id)         → instagram_media_id
```

- `publish_enabled: false` iken **dry-run**: gerçek HTTP isteği atılmaz, sentetik
  `media_id` döner, akışın tamamı (görsel + storage + DB + bildirim) çalışır.
- `access_token` form gövdesinde gönderilir, **asla loglanmaz**.
- İlk sürümde yalnızca **single image** desteklenir; reels/story/carousel için
  yapı genişletilebilir bırakılmıştır.
- Gerçek yayın modunda `public_base_url` public olarak erişilebilir HTTPS adresi
  olmalıdır; localhost config'i yalnızca dry-run içindir.

---

## Telegram callback akışı

**Bildirim (giden)** — `POST {telegram_service}/api/notifications`:

```json
{
  "channel": "telegram",
  "idempotencyKey": "first-approval:42",
  "title": "🔥 Önemli haber bulundu",
  "message": "Başlık: ...\nSkor: 91/100\nKategori: teknoloji",
  "buttons": [
    { "text": "Post Hazırla", "action": "GENERATE_POST", "payload": "<postJobId>" },
    { "text": "Geç",          "action": "SKIP_NEWS",     "payload": "<postJobId>" }
  ]
}
```

**Callback (gelen)** — `POST /api/telegram/callback`:

```json
{ "action": "GENERATE_POST|SKIP_NEWS|SELECT_VARIANT|REGENERATE_VARIANTS|CANCEL",
  "payload": "<postJobId | variantId>", "user": "gokhan" }
```

Unix timestamp ve gövde `timestamp.body` biçiminde `TELEGRAM_CALLBACK_SECRET`
ile HMAC-SHA256 imzalanmalı; timestamp `X-Telegram-Timestamp`, hex imza
`X-Telegram-Signature` header'ında gönderilmelidir. Beş dakikadan eski istekler
ve allowlist dışında kalan `user` değerleri reddedilir.

İki aşamalı onay:

1. **1. aşama** — Önemli haber → `WAITING_FIRST_APPROVAL` → `Post Hazırla` / `Geç`.
2. **2. aşama** — `Post Hazırla` işi kuyruğa alır → `variant_count` kadar alternatif üretilir →
   `WAITING_VARIANT_APPROVAL` → **dinamik** butonlar:

   ```
   [1. Alternatif] [2. Alternatif] ... [N. Alternatif] [Yeniden Üret] [İptal]
   ```

   Her caption seçimden önce ayrı bir Telegram mesajında gösterilir. Buton sayısı
   `variants.length` kadardır (sabit değildir). `SELECT_VARIANT`
   payload'ı `variantId` taşır; seçimle job `APPROVED → PUBLISHING → PUBLISHED`
   yoluna girer.

---

## Yönetim paneli

Uygulama, ayrı bir Node.js servisine ihtiyaç duymayan gömülü bir operasyon
paneli içerir. Tarayıcıdan `http://localhost:8080/login` adresini açıp
`API_AUTH_TOKEN` değeriyle giriş yapın. Token tarayıcıda saklanmaz; başarılı
girişten sonra sekiz saatlik, `HttpOnly` ve `SameSite=Strict` bir oturum cookie'si
kullanılır.

Panelde aşağıdaki ekranlar bulunur:

- **Yayın Masası:** durum özeti, onay bekleyen işler ve bildirim outbox'ı.
- **Post Kuyruğu:** durum, kategori, kanal ve metin filtreleri.
- **Post Detayı:** AI skoru, caption düzenleme, varyant seçimi ve gerçek PNG önizlemesi.
- **Haber Adayları, Kanallar ve Sistem:** salt okunur operasyon görünümleri.

Panelde varyant seçimi işi `READY_TO_PUBLISH` durumunda tutar. Görsel bu aşamada
gerçek renderer ile hazırlanır; operatör caption ve görseli gördükten sonra ayrı
bir **Onayla ve yayınla** aksiyonuyla işi `APPROVED` yayın kuyruğuna alır. Mevcut
Telegram ve JSON API onayları geriye uyumluluk için bu iki adımı tek işlemde
tamamlamaya devam eder.

HTMX ve panel varlıkları binary içine gömülüdür; çalışma zamanında CDN çağrısı
yapılmaz.

---

## Docker Compose kullanımı

PostgreSQL + uygulama birlikte:

```bash
cp .env.example .env        # değerleri düzenleyin
make docker-up              # docker compose up --build -d
make docker-down            # durdur
```

`app` servisi `config.example.yaml` ile başlar ve ortam değişkenlerini
compose'tan alır. Ollama'ya host üzerinden erişmek için
`OLLAMA_BASE_URL=http://host.docker.internal:11434` ayarlıdır.

---

## HTTP API ve örnek curl komutları

| Method | Path                          | Açıklama                              |
|--------|-------------------------------|---------------------------------------|
| GET    | `/live`                       | Liveness                              |
| GET    | `/health`, `/ready`           | DB readiness                          |
| GET    | `/api/accounts`               | Hesapları listele                     |
| POST   | `/api/news/sync`              | Haberleri çek + skorlama kuyruğuna al |
| GET    | `/api/news/candidates`        | Haber adaylarını listele              |
| GET    | `/api/posts`                  | Post joblarını listele                |
| GET    | `/api/posts/{id}`             | Job + alternatiflerini getir          |
| POST   | `/api/posts/{id}/generate`    | Alternatif üretimini başlat           |
| POST   | `/api/posts/{id}/approve`     | Alternatif seç (`{"variantId": N}`)   |
| POST   | `/api/posts/{id}/reject`      | Jobu atla (`SKIPPED`)                 |
| POST   | `/api/posts/{id}/publish`     | Seçili alternatifi yayın kuyruğuna al |
| POST   | `/api/telegram/callback`      | Telegram callback giriş noktası       |
| GET    | `/api/analytics/posts`        | Durum bazlı özet                      |
| GET    | `/static/*`                   | Üretilen görseller (public URL)       |

```bash
# Sağlık
curl -s localhost:8080/health

# Yönetim endpoint'lerinin tamamında Bearer token zorunludur
AUTH="Authorization: Bearer ${API_AUTH_TOKEN}"

# Hesaplar
curl -s -H "$AUTH" localhost:8080/api/accounts | jq

# Haber senkronizasyonu (haber-servisi gerekir)
curl -s -X POST -H "$AUTH" localhost:8080/api/news/sync

# Bir job için alternatif üret
curl -s -X POST -H "$AUTH" localhost:8080/api/posts/1/generate

# Alternatif seç → görsel + (dry-run) yayın
curl -s -X POST localhost:8080/api/posts/1/approve \
  -H "$AUTH" -H 'Content-Type: application/json' -d '{"variantId": 1}'

# Telegram callback simülasyonu: "Post Hazırla"
BODY='{"action":"GENERATE_POST","payload":"1","user":"gokhan"}'
TS=$(date +%s)
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$TELEGRAM_CALLBACK_SECRET" -hex | awk '{print $2}')
curl -s -X POST localhost:8080/api/telegram/callback \
  -H 'Content-Type: application/json' -H "X-Telegram-Timestamp: $TS" \
  -H "X-Telegram-Signature: $SIG" -d "$BODY"

# Analitik
curl -s -H "$AUTH" localhost:8080/api/analytics/posts | jq
```

---

## Status flow

`post_jobs.status` kontrollü bir durum makinesidir
(`internal/post/status.go`). İzin verilmeyen geçişler `409 Conflict` döner.

```
NEW
 └─▶ SCORING_QUEUED ─▶ SCORING
                         ├─▶ WAITING_AI ─▶ SCORING_QUEUED / VARIANTS_QUEUED
                         └─▶ SCORED
        ├─▶ SKIPPED                  (eşik altı / accountFit=skip)
        └─▶ WAITING_FIRST_APPROVAL   (Telegram 1. onay bildirimi)
               ├─▶ SKIPPED            ("Geç")
               └─▶ VARIANTS_QUEUED ─▶ GENERATING_VARIANTS
                      ├─▶ WAITING_AI    (AI yine başarısız → retry)
                      └─▶ WAITING_VARIANT_APPROVAL  (dinamik butonlar)
                             ├─▶ SKIPPED              ("İptal")
                             ├─▶ VARIANTS_QUEUED      ("Yeniden Üret")
                             └─▶ READY_TO_PUBLISH      (panel seçimi + gerçek önizleme)
                                    ├─▶ VARIANTS_QUEUED / SKIPPED
                                    └─▶ APPROVED       (açık yayın onayı)
                                           └─▶ PUBLISHING ─▶ PUBLISHED
                                                          └─▶ FAILED
```

Telegram ve `/api/posts/{id}/approve` geriye uyumluluk için
`READY_TO_PUBLISH → APPROVED` geçişini aynı istek içinde tamamlar.

Terminal durumlar: `PUBLISHED`, `SKIPPED`, `FAILED`.

---

## Görsel üretimi

İlk sürümde AI image generation yoktur; **template tabanlı** 1080×1080 PNG kart
üretilir (`internal/media`). Kartta: kanal/kategori etiketi, haber başlığı,
kaynak, tarih, kısa alt metin ve opsiyonel logo alanı bulunur. Her kategori için
farklı tema (renk + etiket) tanımlıdır.

Arayüzler ileriye dönük tasarlanmıştır:

```go
type MediaRenderer interface {
    RenderPostImage(ctx context.Context, variant post.Variant, news ai.NewsCandidate, account account.Account) (*RenderedMedia, error)
}

type Storage interface {
    Upload(ctx context.Context, filePath string) (*UploadedFile, error)
}
```

Varsayılan `Storage` implementasyonu **local** sürücüdür: dosyayı `base_dir`
altına yazar ve `public_base_url` ile birleştirerek public URL üretir. Bu URL
`GET /static/*` üzerinden servis edilir, böylece Instagram görseli çekebilir.
Üretilen dosyalar `storage.retention_days` süresinden sonra günlük worker ile
temizlenir.

---

## Ekonomi kanalı için özel kurallar

Ekonomi içeriklerinde prompt ve üretim şu kuralları zorlar (`internal/ai/prompts.go`):

- Yatırım tavsiyesi verilmez; "al/sat/kaçırma/garanti kazanç" yönlendirmesi yapılmaz.
- Kesin piyasa/fiyat tahmini yapılmaz; kaynakta olmayan veri eklenmez.
- Panik / manipülatif dil kullanılmaz.
- Gerekirse caption sonunda: *"Bu içerik yatırım tavsiyesi değildir."*

---

## Klasör yapısı

```
ai-social-publisher
├── cmd/server/main.go          # giriş noktası (serve | migrate)
├── internal/
│   ├── account/                # social_accounts repo + config sync
│   ├── ai/                     # provider chain (tgpt, ollama), prompts, parse
│   ├── approval/               # uçtan uca orkestrasyon
│   ├── config/                 # YAML + ${ENV} + defaults + validation
│   ├── database/               # pgx pool + goose migrate
│   ├── http/                   # chi router + handlers
│   ├── instagram/              # Graph API publisher (dry-run destekli)
│   ├── media/                  # template renderer (PNG kart)
│   ├── news/                   # candidate/score repo + haber-servisi client
│   ├── outbox/                 # dayanıklı Telegram teslimatı + backoff
│   ├── post/                   # job/variant/publish_log repo + status FSM
│   ├── scheduler/              # in-process worker'lar
│   ├── storage/                # Storage arayüzü + local sürücü
│   └── telegram/               # bildirim client + callback tipleri
├── migrations/                 # goose SQL (embed)
├── templates/                  # kanal şablon kaynakları (ileride)
├── config.example.yaml
├── docker-compose.yml · Dockerfile · Makefile · .env.example
```

---

## Test

```bash
make test        # go test ./...
make test-race   # race detector
make vet         # go vet ./...
make vuln        # govulncheck ./...
```

Birim testlere ek olarak API auth/HMAC ve gerçek PostgreSQL üzerinde atomik
worker claim entegrasyon testi bulunur. CI; format, vet, race detector, migration,
vulnerability scan, binary ve container build adımlarını çalıştırır.

---

## Hata yönetimi ilkeleri

- AI provider hataları sistemi durdurmaz → bounded backoff ile `WAITING_AI`.
- Instagram publish hataları `publish_logs` tablosuna yazılır, job `FAILED` olur.
- Telegram bildirimleri outbox üzerinden retry edilir; 10 denemeden sonra dead-letter olur.
- Worker'lar işleri atomik claim eder; aynı post iki worker tarafından yayınlanamaz.
- Yarım kalan AI işleri geri kazanılır. Sonucu belirsiz Instagram isteği otomatik
  tekrar edilmez; olası duplicate yerine manuel uzlaştırma istenir.
- JSON parse hatasında fallback provider denenir.
- Duplicate haberler tekrar işlenmez (`external_news_id` + job tekilliği).
- Tüm timeout'lar config'ten gelir.
- Token/hassas veri loglanmaz; panic yerine kontrollü hata yönetimi + worker'larda
  panic recovery kullanılır.
