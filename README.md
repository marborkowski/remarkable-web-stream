# reMarkable Live — podgląd ekranu przez przeglądarkę (Cloudflare Tunnel)

Otwierasz adres WWW z dowolnego komputera, wpisujesz hasło i widzisz na żywo to,
co rysujesz na reMarkable. Wszystko działa **na samym tablecie** jako trzy usługi
systemd; Mac jest potrzebny tylko do wdrożenia.

```
przeglądarka ──HTTPS──> Cloudflare ──tunel──> cloudflared (na rM)
                                                 │ http://127.0.0.1:8080
                                                 ▼
                                      rmshare (proxy, formularz hasła, cookie sesji)
                                                 │ http://127.0.0.1:2011
                                                 ▼
                                      goMarkableStream (czyta framebuffer, strumień klatek + UI)
```

* **goMarkableStream** (gotowy projekt, [owulveryck/goMarkableStream](https://github.com/owulveryck/goMarkableStream))
  robi całą ciężką robotę: czyta ekran z pamięci `xochitl` i serwuje stronę z podglądem WebGL.
  Uruchamiany tylko na `127.0.0.1`, z wyłączonym własnym logowaniem (`-unsafe`, `RK_JWT_ENABLED=false`),
  więc z zewnątrz nie da się go dosięgnąć inaczej niż przez proxy.
  Budowany ze źródeł z własną łatką (`gms/pointer_arm64.patch`): oryginalne wykrywanie bufora ekranu
  na Paper Pro działa tylko po zimnym starcie, a po restarcie `xochitl` zawieszało się w pętli;
  łatka szuka bufora deterministycznie: skanuje mapowania pamięci xochitl w poszukiwaniu chunku malloc
  o rozmiarze klatki, którego zawartość wygląda jak nieprzezroczysty obraz BGRA; stara metoda zostaje jako zapas.
* **rmshare** (`proxy/main.go`, własny kod w Go) stoi przed nim: bez ważnego cookie pokazuje formularz hasła,
  po zalogowaniu przepuszcza cały ruch, w tym długie odpowiedzi `/stream` i SSE `/events`.
  Hasło jest **zahardkodowane w stałej `Password`** w `proxy/main.go`.
* **cloudflared** wystawia proxy do internetu. Bez konta Cloudflare dostajesz losowy adres
  `*.trycloudflare.com` (zmienia się po restarcie); z kontem i domeną — stały adres.

## Wymagania

* reMarkable 2 lub Paper Pro z trybem deweloperskim i SSH (`root@10.11.99.1` przez USB).
* Na Macu: `go`, `curl`, `patch`, `ssh`. Klucz SSH na urządzeniu — jednorazowo:
  ```bash
  ssh-copy-id root@10.11.99.1
  ```
  (hasło roota znajdziesz na tablecie: Settings → Help → Copyrights and licenses, sekcja GPLv3.)

## Szybki start (quick tunnel, bez konta Cloudflare)

1. Zmień hasło: w `proxy/main.go` edytuj `const Password = "..."` (obecnie `remarkable-live-2026`).
   Hasło celowo **nie** jest drukowane w dokumencie z QR na tablecie.
2. Wdróż:
   ```bash
   ./deploy.sh
   ```
   Skrypt wykrywa architekturę tabletu, buduje proxy i goMarkableStream (ze źródeł, z łatką),
   pobiera cloudflared, kopiuje wszystko do `/home/root/rmshare/` i włącza usługi. Na końcu wypisuje `ADRES: https://xxxx.trycloudflare.com`.
3. Otwórz adres, wpisz hasło. W menu podglądu (ikona ⚙) możesz przełączyć orientację itp.

## Gdzie znaleźć aktualny adres (quick tunnel zmienia go po każdym restarcie)

1. **Na ekranie tabletu**: po każdej zmianie adresu proxy zapisuje w bibliotece dokument
   **„reMarkable Live – adres”** (przypięty, w Ulubionych) z adresem i kodem QR — zeskanuj go
   telefonem albo przepisz. Żeby tablet zobaczył nowy dokument, proxy restartuje `xochitl`
   (ok. 10 s, zaraz po uruchomieniu tunelu). Jeśli tego nie chcesz, wpisz w `tunnel.env`
   `PUBLISH_DOC=0`; adres nadal będzie w pozostałych miejscach.
2. **Lokalnie w przeglądarce**: `http://10.11.99.1:8080/url` (USB) lub
   `http://<IP-tabletu>:8080/url` — po zalogowaniu strona z adresem i QR;
   `.../url?format=txt` daje sam adres.
3. **Przez SSH**:
   ```bash
   ssh root@10.11.99.1 cat /home/root/rmshare/tunnel-url.txt
   ```

### Watchdog tunelu

Quick tunnel po dłuższym braku sieci (tablet na baterii w suspendzie przez kilka godzin) jest
kasowany po stronie Cloudflare, a cloudflared w nieskończoność próbuje go zarejestrować
(`Unauthorized: Tunnel not found`) zamiast założyć nowy. `run-tunnel.sh` wykrywa to po logu
i po endpointcie `/ready` cloudflared (503 przez 2 min mimo działającego internetu), kasuje
stary adres, ubija cloudflared i kończy się — systemd uruchamia świeży tunel, nowy adres
trafia do dokumentu na tablecie. Próg: `TUNNEL_DEAD_AFTER=120` (sekundy) w `tunnel.env`.

## Stały adres bez domeny: Tailscale Funnel (zalecane)

1. Załóż darmowe konto na [tailscale.com](https://tailscale.com) (logowanie Google/GitHub/e‑mail).
2. W katalogu projektu utwórz `tunnel.env` (jest w `.gitignore`):
   ```
   TUNNEL=tailscale
   TS_HOSTNAME=remarkable
   ```
   Opcjonalnie `TS_AUTHKEY=tskey-auth-...` (Settings → Keys → Generate auth key; zaznacz
   *Reusable*), wtedy tablet loguje się sam. Bez klucza skrypt wypisze link do zalogowania.
3. `./deploy.sh` — skrypt pobierze binarki Tailscale (`tailscaled` w trybie userspace, bo
   reMarkable nie ma `/dev/net/tun`) i wgra je na tablet. Na końcu wypisze:
   * `ZALOGUJ URZADZENIE W TAILSCALE: https://login.tailscale.com/a/...` — otwórz, zatwierdź;
   * `WLACZ FUNNEL W TAILSCALE: https://login.tailscale.com/f/funnel?...` — otwórz, zatwierdź
     (jednorazowo dodaje atrybut `funnel` do polityki tailnetu). Włącz też
     **DNS → HTTPS Certificates** w konsoli, jeśli nie jest włączone.
4. Po chwili pojawi się `ADRES: https://remarkable.<tailnet>.ts.net` — stały, trafia też do
   dokumentu na tablecie. Publiczny rekord DNS dla tej nazwy Tailscale tworzy dopiero po
   włączeniu Funnel i propagacja trwa od kilku do kilkunastu minut (do tego czasu przeglądarka
   zgłosi „nie znaleziono serwera”). Pierwsze wejście może potrwać kilkanaście sekund
   (wystawienie certyfikatu Let's Encrypt).

Szczegóły działania (`device/run-tunnel.sh`, tryb `tailscale`): `tailscaled` dostaje `--statedir`
(bez niego nie ma gdzie trzymać certyfikatów i Funnel zwraca „no TailscaleVarRoot”);
`tailscale up` uruchamiane jest tylko raz, bo każde kolejne unieważnia poprzedni link logowania;
`tailscale funnel` wywoływane jest z limitem czasu, bo bez włączonego Funnel czeka w nieskończoność.

Stan można sprawdzić przez SSH: `/home/root/rmshare/tailscale --socket=/home/root/rmshare/tailscale-state/tailscaled.sock status`.
Linki do zalogowania / włączenia Funnel są też w `/home/root/rmshare/tailscale-login-url.txt`
i `tailscale-funnel-url.txt`, dopóki są potrzebne.

## Stały adres (tunel nazwany, konto Cloudflare + domena)

1. W [Cloudflare Zero Trust](https://one.dash.cloudflare.com) → Networks → Tunnels → **Create a tunnel**
   (typ Cloudflared), nazwij np. `remarkable`.
2. Skopiuj token z komendy instalacyjnej (fragment po `--token`).
3. W zakładce **Public Hostname** dodaj np. `rm.twojadomena.pl` → Service `HTTP`, URL `localhost:8080`.
4. Na Macu, w katalogu projektu, utwórz plik `tunnel.env` (jest w `.gitignore`):
   ```
   TUNNEL_TOKEN=eyJhIjoi...
   ```
   Opcjonalnie dopisz `PUBLIC_URL=https://rm.twojadomena.pl`, żeby ten adres też pojawił się
   w `/url` i w dokumencie na tablecie.
5. `./deploy.sh` — skrypt `run-tunnel.sh` na urządzeniu wykryje token i użyje tunelu nazwanego.

Alternatywnie możesz położyć w katalogu `cloudflared/` własny `config.yml` + plik credentials
(z `cloudflared tunnel create`); `run-tunnel.sh` użyje ich, gdy nie ma `tunnel.env`.

Dodatkowo możesz w Cloudflare włączyć Access (logowanie e-mailem) przed hostem —
wtedy masz dwie warstwy: Cloudflare Access + hasło w proxy.

## Uśpienie tabletu

Paper Pro podłączony do USB nie wchodzi w prawdziwy suspend (ładowarka trzyma blokadę
wybudzenia), więc po uśpieniu ekranu procesy i Wi‑Fi dalej działają. Żeby nikt nie oglądał
„uśpionego” tabletu, proxy śledzi w journalu xochitl komunikat
`Changing display state from Normal to DeepSleep` (i z powrotem):

* w chwili uśpienia zrywa aktywne strumienie i zdarzenia pióra, a kolejne żądania
  `/stream`, `/screenshot`, `/events`, `/gestures` dostają **503**;
* strona (łatka `gms/client_sleep.patch`) zasłania ostatnią klatkę nakładką „Tablet jest uśpiony”
  i co 5 s sprawdza, czy tablet się obudził — po wybudzeniu obraz wraca sam;
* przy utracie połączenia (tablet na baterii w suspendzie, brak Wi‑Fi) nakładka pokazuje
  „Brak połączenia z tabletem” i strona ponawia próby bez końca.

Stan można sprawdzić po zalogowaniu pod `/state` (JSON `{"awake":true|false}`).
Wyłączenie: `-sleep-watch=false` w `device/rmshare-proxy.service`.

## Alternatywy dla Cloudflare (ocena)

* **ngrok (darmowy)** — odpada do streamingu ekranu: limit **1 GB transferu wychodzącego
  na miesiąc** (to rzędu kilku godzin oglądania), do tego ngrok wstawia stronę pośrednią
  (interstitial) przed każdą sesją przeglądarki na darmowym planie. Zaletą byłaby stała
  subdomena `*.ngrok-free.app`.
* **Cloudflare, tunel nazwany** — ta sama infrastruktura co quick tunnel (więc ta sama
  wydajność), ale stały adres i brak problemu z kasowaniem tunelu po dłuższym offline.
  Wymaga konta i własnej domeny.
* **Tailscale Funnel** (wdrożone, patrz wyżej) — darmowy, stały adres `*.ts.net`, nie wymaga
  domeny. Ruch idzie przez relaye Tailscale, TLS kończy się na tablecie.
* **Tailscale bez Funnel** (komputer widza też w tailnecie) — najlepsza wydajność, bo
  połączenie WireGuard jest bezpośrednie i nic nie pośredniczy; adres stały; ale nie
  otworzysz podglądu z dowolnego komputera bez zainstalowanego Tailscale.

Zanim zmienisz tunel, porównaj płynność lokalnie (`http://10.11.99.1:8080` po USB) z tunelem.
Jeśli lokalnie jest płynnie, a przez tunel nie — winny jest pośrednik; jeśli oba są podobne,
ogranicza Wi‑Fi tabletu lub samo kodowanie klatek.

## Automatyczny obrót podglądu

Bufor ekranu tabletu jest zawsze w pionie, a xochitl obraca w nim interfejs, gdy obrócisz
tablet. Proxy czyta z logu xochitl linie `Setting new orientation Landscape` (i pozostałe:
`Portrait`, `InvertedLandscape`, `InvertedPortrait`) i podaje bieżącą orientację w `/state`.
Strona (łatka `gms/client_orientation.patch`) odpytuje `/state?wait=<wersja>` w trybie
long-poll: żądanie czeka do 25 s i wraca od razu po zmianie, więc obrót widać po ułamku sekundy,
a w spoczynku ruch to jedno małe żądanie na 25 s.

* Przycisk **Auto-rotate** w lewym menu (skrót **A**) włącza/wyłącza śledzenie; domyślnie
  włączone, wybór zapamiętuje przeglądarka.
* Ręczne kliknięcie **Rotate** (skrót **R**) wyłącza Auto-rotate, żeby nie walczyć z wyborem
  użytkownika.
* Mapowanie orientacji na widok jest w tablicy `ORIENTATION_VIEWS` (łatka
  `gms/client_orientation.patch`). Na Paper Pro `Landscape` = obrót 90° + odwrócenie,
  `InvertedLandscape` = sam obrót 90°.
* Nowy obrót jest nakładany razem z pierwszą klatką, w której obraz faktycznie się zmienił
  (awaryjnie po 2 s), żeby nigdy nie obracać starego obrazu.
* Uwaga: xochitl sam obraca ekran, gdy czujnik wykryje przechylenie, także w trakcie pisania
  na kolanach. Podgląd wiernie to odtwarza; jeśli przeszkadza, zablokuj obrót na tablecie.

## Odświeżanie obrazu bez pióra

Oryginalny goMarkableStream wysyła klatki tylko podczas pisania lub dotyku i zasypia 2 s
po ostatnim ruchu. Zmiany, które xochitl robi sam (dokument doładowuje się po kilku
sekundach, obrót, przejście ekranu), docierały więc dopiero przy następnym pociągnięciu
piórem. Łatka `gms/stream_idle_refresh.patch` sprawia, że w spoczynku strumień nadal
sprawdza ekran co 0,5 s; niezmieniona klatka to kilkubajtowa pusta delta. Koszt: ok. 15–20%
jednego rdzenia (z czterech) na czas oglądania, gdy nikt nie pisze; bez otwartego podglądu
strumień nie działa w ogóle.

## Rysowanie na tablecie z przeglądarki (zdalne pióro)

Przycisk **Draw** w lewym menu (skrót **D**) zamienia podgląd w powierzchnię do rysowania
myszą, palcem lub rysikiem. Ruchy są przeliczane na natywne współrzędne tabletu (z
uwzględnieniem obrotu podglądu) i wysyłane przez WebSocket `/pen` do proxy, które wstrzykuje
je jako zdarzenia pióra do `/dev/input/event2`. xochitl czyta to urządzenie bezpośrednio,
więc rysuje **narzędziem aktualnie wybranym na tablecie** (pióro, marker, gumka…).

* Nacisk jest stały: 50% (`penPressureFraction` w `proxy/pen.go`).
* Naraz rysuje jedna osoba; zerwane połączenie lub 1,5 s bez ruchu zawsze podnosi pióro.
* Gdy tablet śpi, zdarzenia są odrzucane.
* WebSocket przyjmuje tylko połączenia z tej samej strony (sprawdzanie `Origin`) i wymaga
  zalogowania.
* Wyłączenie: `-pen-device=` w `device/rmshare-proxy.service`.
* Diagnostyka: `tools/pentool` (`info` — zakresy osi, `record` — zrzut zdarzeń z pióra,
  `line` — testowa kreska).

## Sygnał na tablecie: ktoś ogląda

xochitl nie ma lokalnego API powiadomień (jego pasek powiadomień zasila tylko chmura
reMarkable), a pikseli dopisanych z zewnątrz e-ink nie odświeży. Proxy sygnalizuje więc
widzów **mrugnięciem podświetlenia** (front light) i zawsze przywraca Twój poziom jasności:

* **dwa krótkie mrugnięcia** — pierwszy widz połączył się ze strumieniem (po 2 s trwania
  połączenia, więc przypadkowe odświeżenie strony nie mruga); można zaczynać,
* **jedno długie mrugnięcie** — ostatni widz odszedł (po 15 s bez widzów, żeby chwilowe
  zerwania nie mrugały).

Przy jasnym podświetleniu mrugnięcie to przygaszenie, przy ciemnym lub wyłączonym —
rozjaśnienie. Gdy tablet śpi, nic nie mruga. Liczba aktywnych widzów jest w `/state`
(pole `viewers`). Wyłączenie: `-notify-light=` w `device/rmshare-proxy.service`.

## Obsługa na urządzeniu

```bash
ssh root@10.11.99.1
systemctl status rmshare-stream rmshare-proxy rmshare-tunnel
journalctl -u rmshare-stream -u rmshare-proxy -u rmshare-tunnel -f
sh /home/root/rmshare/uninstall.sh        # wyłącza i usuwa usługi
```

Podgląd działa też lokalnie bez tunelu: `http://10.11.99.1:8080` (USB) lub
`http://<IP-tabletu-w-Wi-Fi>:8080` — również za hasłem.

## Uwagi

* **Aktualizacja systemu reMarkable** nadpisuje `/etc/systemd/system`, ale zostawia `/home/root`.
  Po aktualizacji wystarczy `ssh root@10.11.99.1 'sh /home/root/rmshare/install.sh'` (lub `./deploy.sh`).
* Sesje logowania trzymane są w pamięci proxy: restart usługi/tabletu = ponowne wpisanie hasła.
  Czas ważności cookie: 30 dni (`SessionTTL`).
* Ochrona przed zgadywaniem: 1,5 s opóźnienia po błędnym haśle, blokada po 20 błędach w 15 minut.
* Zużycie zasobów na rM2: goMarkableStream ok. 20–40 MB RAM, cloudflared ok. 30–50 MB; nie zauważysz.
* Strumień klatek idzie przez Cloudflare jako zwykła odpowiedź HTTP chunked — działa na darmowym planie.
  Przy dłuższej bezczynności przeglądarka może zerwać SSE zdarzeń pióra; klient sam się łączy ponownie.
* Wersje: goMarkableStream `v1.4.0`, cloudflared `2026.10.1` (zmienne `GMS_VERSION`, `CF_VERSION` w `deploy.sh`).
* Oryginalny goMarkableStream obsługuje **jednego widza naraz** (drugi dostaje 429, a klient
  pokazuje „Rate limited” na stałe). Łatka `gms/stream_preempt.patch` daje każdemu połączeniu
  własny enkoder delta i dopuszcza **3 równoczesnych widzów** (np. karta lokalna + karta przez
  tunel); dopiero czwarty przejmuje strumień, zrywając pozostałe. Bez tego dwie otwarte karty
  odbierały sobie strumień co 1–2 s („reconnect co kilka sekund”).
* „Bad gateway 502” po zalogowaniu oznacza, że goMarkableStream nie działa:
  `journalctl -u rmshare-stream -n 30`.
* Przetestowane na reMarkable Paper Pro (firmware 6.0.106). reMarkable 2 powinien działać
  (goMarkableStream wspiera go oficjalnie), ale nie był sprawdzany.

## Pliki

```
proxy/main.go, proxy/login.html   proxy z hasłem (Go)
proxy/urldoc.go                    strona /url + generowanie PDF z adresem i QR do biblioteki
device/*.service                   usługi systemd: stream, proxy, tunnel
device/run-tunnel.sh               wybór trybu tunelu (tailscale / cloudflared token / config.yml / quick) + watchdog
device/install.sh, uninstall.sh    instalacja usług na tablecie
gms/build-gms.sh, gms/*.patch      build goMarkableStream ze źródeł z łatkami (Paper Pro, wielu widzów, uśpienie, auto-obrót, odświeżanie w spoczynku, zdalne pióro)
gms/series                         kolejność nakładania łatek
proxy/sleep.go                     watcher logu xochitl: uśpienie (blokada strumienia) i orientacja; /state z long-pollem
proxy/viewers.go                   liczenie widzów i sygnał mrugnięciem podświetlenia
proxy/pen.go                       zdalne pióro: WebSocket /pen -> zdarzenia evdev
tools/pentool/                     narzędzie diagnostyczne pióra (info / record / line)
deploy.sh                          build + pobranie + wgranie + instalacja (z Maca)
dist/                              binarki (cache, poza repo)
```
