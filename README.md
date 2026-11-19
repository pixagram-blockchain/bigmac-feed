# bigmac-feed

Price feed publisher for [Pixagram](https://pixagram.com) witnesses.

The feed values 1 PXS at the price of one Big Mac in the country the feeder runs in, and publishes how many PIXA that is:

```
1.000 PXS = N PIXA        N = Big Mac price / PIXA fair value, both in the Big Mac's currency
```

## Currency agnostic

No currency is privileged. The Big Mac is priced in the **local currency** of its country (6.99 EUR in Germany, 500 JPY in Japan, 5,700 KRW in South Korea), and PIXA is valued on **any market**, in whatever currency that market quotes it (USDT on Coinstore today). When the two currencies differ, the PIXA fair value is converted into the Big Mac's currency at today's rate. N is a pure ratio of a Big Mac to PIXA: routing it through USD, CHF or EUR gives the same result. The chain's median across witnesses then aims PXS at a world-median Big Mac.

Stablecoins count as the fiat they track (USDT, USDC, FDUSD, DAI… → USD; EURC, EURT → EUR; XCHF, ZCHF → CHF). FX rates come from the ECB reference rates via [Frankfurter](https://frankfurter.dev) (about 30 currencies) and, for the other Big Mac currencies (ARS, EGP, VND, PKR, UAH…), from [ExchangeRate-API](https://www.exchangerate-api.com) ([Rates By Exchange Rate API](https://www.exchangerate-api.com)). Neither needs a key. The last good rate is reused for up to `--fx-max-age` if both are down.

## Price data

The Big Mac prices are Pixa's own, kept in this repository. Until v1.3.0 the feed read The Economist's Big Mac index.

| File | Holds | Read by |
|---|---|---|
| [`latest.csv`](latest.csv) | The **pick-up price**: what the restaurant itself charges at the counter, at the kiosk or in the McDonald's app | the feed, and [pixa.org](https://pixa.org/bigmac.html) |
| [`delivery.csv`](delivery.csv) | The **delivery price**: what delivery apps such as Uber Eats list, before their fees | pixa.org only, for comparison |

The two are different prices. A delivery app has its own price list for the same restaurant, usually higher: in Germany the usual pick-up price is 6.99 EUR while Uber Eats lists 7.69 EUR in Berlin. The feed uses the pick-up price only.

Both files have the same columns:

| Column | Meaning |
|---|---|
| `country_code` | ISO 3166-1 alpha-2 code. One row per country, in alphabetical order |
| `currency_code` | ISO 4217 code of the currency the prices are in, the country's own |
| `low`, `high` | The lowest and the highest price observed |
| `median` | The median of the prices observed. This is the price the feed uses |
| `date` | The day the prices were observed, `YYYY-MM-DD`. For `delivery.csv`, the day the page was read: a page read through a search engine can be an older copy |
| `sources` | Where each price was read, separated by ` \| ` |

The rules:

- **The burger on its own**, never the menu or meal. In France that is the Big Mac under "Nos produits à l'unité".
- **The price a customer pays**, taxes included where the country shows prices that way.
- **A source someone else can check**: McDonald's own price list or announcement, a count of restaurant prices that says how it was made, a press report of a restaurant price, or a price a witness read in the McDonald's app or at the counter, with the place and the day.
- **No row without a source.** A country nobody has a pick-up price for is left out, and no other country's price stands in for it.

`go test ./...` checks both files against these rules.

### Reporting a price

Read the price of a Big Mac on its own at your restaurant: in the McDonald's app, start an order to collect at that restaurant; or read the menu board. Then open a pull request that adds or updates your country's row in `latest.csv`, with the restaurant, the day and the price in `sources`, and run `go test ./...`. Several restaurants make a better row than one: `low` and `high` are the extremes, `median` the middle.

Until your country has a row, state your price to the feed yourself with `--bigmac-price`.

## Fair value, not the last trade

The last trade is not a price. On a thin book, a single 1-PIXA market order can print anywhere: on 30 Oct 2026 the last PIXAUSDT trade was a 1-PIXA sell at 0.0367 while the book stood at 0.1339 bid / 0.145 ask. A feed that follows it lets anyone move the PXS conversion rate for a few cents. So the feed reads the **order book** and values PIXA at its fair value:

1. **Impact prices.** The average price of selling `--impact-size` PIXA (default 1000) into the bids, and of buying as many from the asks. If a side holds less, the feed uses what that side holds. A dust order at the top of the book barely counts.
2. **Impact mid.** The average of the two impact prices.
3. **Bounded by the spread.** The impact mid is held between the best bid and the best ask, since a fair price outside the spread could be traded against at once. This also keeps a thin side, such as a few asks far above the market, from dragging the value away.

The book must be two-sided and not crossed, and its spread must be at most `--max-spread` of the mid (default 25 %). Otherwise there is no fair value. With several `--pair` markets, each is converted into the Big Mac's currency and the median is used.

If no market gives a fair value, nothing is published for that round, and the chain keeps the witness's previous feed. `--token-price` can set a placeholder for the time before a listing. It is off by default.

## What it does

The feeder picks the Big Mac:

1. `--bigmac-price`, if set (a price you observe yourself, e.g. `"7.30 CHF"`);
2. otherwise the row of `--country`, if set;
3. otherwise the row of the country of the server's public IP, looked up via [ipinfo.io](https://ipinfo.io).

No country stands in for another. If the country has no row in `latest.csv`, or the server's country cannot be told, the round fails with a message saying so, nothing is published, and the chain keeps the witness's previous feed. `--country=EUZ` asks for the euro area as a whole: the median of the euro area countries that have a row.

Then every hour it:

1. reads that country's pick-up price and currency from [`latest.csv`](latest.csv) on the `main` branch of this repository. If the file cannot be fetched, the feed uses the last copy it fetched, and before any fetch succeeded, the copy built into the binary;
2. reads the PIXA fair value from each market (`coinstore:PIXAUSDT` by default);
3. converts it into the Big Mac's currency if needed;
4. publishes `1.000 PXS = <Big Mac price / PIXA fair value> PIXA` for each witness.

Every round logs the full derivation, for example:

```
Big Mac in DE = 6.99 EUR (pick-up price as of 2026-10-24) | PIXA = 0.145 USDT [coinstore PIXAUSDT: bid 0.1339 / ask 0.145, 1000-PIXA impact 0.1339 / 0.551543] = 0.131914 EUR | FX USD→EUR 0.90975 (ECB 2026-11-19) → 1 PXS = 52.989 PIXA
```

Every network request times out after 60 s. A failed round is logged and retried at the next interval.

## Run

```bash
docker run -d --name bigmac-feed --restart unless-stopped \
  -e WITNESS_WIF=<active key> \
  pixarex/bigmac-feed:v1.4.0 \
  --witness=<witness name>
```

Add `--country=DE` (or any other country code) to use a fixed country instead of the detected one, or `--bigmac-price="7.80 CHF"` to use the price you pay.

To see what would be published without broadcasting (no key needed):

```bash
docker run --rm pixarex/bigmac-feed:v1.4.0 --dry-run --once --witness=<witness name>
```

Run this before switching from v1.3.0: if your country has no row yet, the output says so, and you need `--bigmac-price`.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--witness` | `initminer` | Witness account(s), comma-separated |
| `--wif` | `WITNESS_WIF` env | Active key(s), in the same order as `--witness` |
| `--rpc` | `https://api.pixagram.com` | Node to broadcast through |
| `--chain-id` | read from `--rpc` | Chain ID in hex |
| `--country` | detected from IP | Country whose Big Mac price to use: ISO 3166 code (`CN`, `CHN`), or `EUZ` for the median of the euro area countries in the price file |
| `--bigmac-price` | | Pick-up price of a Big Mac with its currency, e.g. `"7.30 CHF"`; overrides `--country` and the price file |
| `--bigmac-url` | `latest.csv` on this repository's `main` | Price file; empty uses only the copy built into the binary |
| `--pair` | `coinstore:PIXAUSDT` | PIXA market(s), repeatable or comma-separated: `coinstore:SYMBOL`, or `coinstore:SYMBOL:QUOTE` when the quote currency cannot be read from the symbol |
| `--impact-size` | `1000` | Order size in PIXA used to read the fair value; `0` = mid of the best bid and ask |
| `--max-spread` | `0.25` | Widest spread, relative to the mid, at which a book has a fair value; `0` disables |
| `--token-price` | `0` (off) | Placeholder PIXA price, used only when no market gives a fair value |
| `--token-currency` | `USD` | Currency of `--token-price` |
| `--fx-url` | `https://api.frankfurter.dev/v1` | ECB rates; empty disables |
| `--fx-fallback-url` | `https://open.er-api.com/v6` | Rates for currencies the ECB does not cover; empty disables |
| `--fx-max-age` | `96h` | How long a cached FX rate may be reused while both FX sources are down |
| `--interval` | `1h` | How often to publish |
| `--once` | `false` | Publish once and exit |
| `--dry-run` | `false` | Compute and log the feed without broadcasting; no key needed |

## Changes in v1.4.0

- The Big Mac price comes from Pixa's own file, `latest.csv`, not from The Economist's index. It holds the pick-up price per country; delivery-app prices are kept apart in `delivery.csv` and are not used by the feed.
- Every euro area country has its own row. `--country=EUZ` still works and gives the median of the euro area rows.
- A country without a row no longer falls back to the US price, and a server whose country cannot be detected no longer counts as being in the US. The round fails instead, and the message says to set `--bigmac-price` or `--country`.
- If `latest.csv` cannot be fetched, the feed keeps publishing from the last copy it fetched, or from the copy built into the binary.
- `--bigmac-url` now points at a price file in the format above. A URL of The Economist's CSV is rejected.

## Changes in v1.3.0

- PIXA is valued at the order book's fair value, not at the last trade.
- The Big Mac is priced in its local currency (`local_price`), not converted to USD at the survey's exchange rate (`dollar_price`). PIXA is converted into that currency at today's rate instead.
- `--pair` selects the market(s), in any quote currency; `--bigmac-price` lets a witness declare the price it pays.
- `--token-price` is now off by default and only applies when no market gives a fair value. Previously it defaulted to 0.12 USD and was used whenever the pair had not traded.
- `--dry-run`.

## Build

```bash
docker build -t bigmac-feed .
go test ./...
```

`latest.csv` is built into the binary, so a new price reaches witnesses in two ways: at once through the published file, and with the next release as the built-in copy.

`chain.go` holds the transaction building, signing and broadcast; `bigmac.go` the price file and the Big Mac price; `market.go` the order book and fair value; `fx.go` the currency conversion; `feed.go` the ratio; `main.go` wires them.
