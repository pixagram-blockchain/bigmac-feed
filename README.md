# bigmac-feed

Price feed publisher for [Pixagram](https://pixagram.com) witnesses.

The feed values 1 PXS at the price of one Big Mac in the country the feeder runs in, and publishes how many PIXA that is:

```
1.000 PXS = N PIXA        N = Big Mac price / PIXA fair value, both in the Big Mac's currency
```

## Currency agnostic

No currency is privileged. The Big Mac is priced in the **local currency** of its country (7.30 CHF in Switzerland, 500 JPY in Japan, 6.22 USD in the US), and PIXA is valued on **any market**, in whatever currency that market quotes it (USDT on Coinstore today). When the two currencies differ, the PIXA fair value is converted into the Big Mac's currency at today's rate. N is a pure ratio of a Big Mac to PIXA: routing it through USD, CHF or EUR gives the same result. The chain's median across witnesses then aims PXS at a world-median Big Mac.

Stablecoins count as the fiat they track (USDT, USDC, FDUSD, DAI… → USD; EURC, EURT → EUR; XCHF, ZCHF → CHF). FX rates come from the ECB reference rates via [Frankfurter](https://frankfurter.dev) (about 30 currencies) and, for the other Big Mac currencies (ARS, EGP, VND, PKR, UAH…), from [ExchangeRate-API](https://www.exchangerate-api.com) ([Rates By Exchange Rate API](https://www.exchangerate-api.com)). Neither needs a key. The last good rate is reused for up to `--fx-max-age` if both are down.

## Fair value, not the last trade

The last trade is not a price. On a thin book, a single 1-PIXA market order can print anywhere: on 30 Oct 2026 the last PIXAUSDT trade was a 1-PIXA sell at 0.0367 while the book stood at 0.1339 bid / 0.145 ask. A feed that follows it lets anyone move the PXS conversion rate for a few cents. So the feed reads the **order book** and values PIXA at its fair value:

1. **Impact prices.** The average price of selling `--impact-size` PIXA (default 1000) into the bids, and of buying as many from the asks. If a side holds less, the feed uses what that side holds. A dust order at the top of the book barely counts.
2. **Impact mid.** The average of the two impact prices.
3. **Bounded by the spread.** The impact mid is held between the best bid and the best ask, since a fair price outside the spread could be traded against at once. This also keeps a thin side, such as a few asks far above the market, from dragging the value away.

The book must be two-sided and not crossed, and its spread must be at most `--max-spread` of the mid (default 25 %). Otherwise there is no fair value. With several `--pair` markets, each is converted into the Big Mac's currency and the median is used.

If no market gives a fair value, nothing is published for that round, and the chain keeps the witness's previous feed. `--token-price` can set a placeholder for the time before a listing. It is off by default.

## What it does

On startup the feeder picks the Big Mac:

1. `--bigmac-price`, if set (a price you observe yourself, e.g. `"7.30 CHF"`);
2. otherwise `--country`, if set;
3. otherwise the country of the server's public IP, looked up once via [ipinfo.io](https://ipinfo.io);
4. otherwise, if that lookup fails, the United States.

Then every hour it:

1. reads that country's local Big Mac price and currency from the latest survey of The Economist's [Big Mac index](https://www.economist.com/big-mac-index). Euro area countries use the `Euro area` price in EUR. A detected country that is not in the latest survey uses the US price; a `--country` that is not in it fails the round;
2. reads the PIXA fair value from each market (`coinstore:PIXAUSDT` by default);
3. converts it into the Big Mac's currency if needed;
4. publishes `1.000 PXS = <Big Mac price / PIXA fair value> PIXA` for each witness.

Every round logs the full derivation, for example:

```
Big Mac in Switzerland = 7.3 CHF (as of 2026-07-01) | PIXA = 0.145 USDT [coinstore PIXAUSDT: bid 0.1339 / ask 0.145, 1000-PIXA impact 0.1339 / 0.551543] = 0.120143 CHF | FX USD→CHF 0.82857 (ECB 2026-10-29) → 1 PXS = 60.761 PIXA
```

Every network request times out after 60 s. A failed round is logged and retried at the next interval.

## Run

```bash
docker run -d --name bigmac-feed --restart unless-stopped \
  -e WITNESS_WIF=<active key> \
  pixadock/bigmac-feed:v1.3.0 \
  --witness=<witness name>
```

Add `--country=CH` (or any other country code) to use a fixed country instead of the detected one, or `--bigmac-price="7.80 CHF"` to use the price you pay.

To see what would be published without broadcasting (no key needed):

```bash
docker run --rm pixadock/bigmac-feed:v1.3.0 --dry-run --once --witness=<witness name>
```

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--witness` | `initminer` | Witness account(s), comma-separated |
| `--wif` | `WITNESS_WIF` env | Active key(s), in the same order as `--witness` |
| `--rpc` | `https://api.pixagram.com` | Node to broadcast through |
| `--chain-id` | read from `--rpc` | Chain ID in hex |
| `--country` | detected from IP | Country whose Big Mac price to use: ISO 3166 code (`CN`, `CHN`), or `EUZ` for the euro area |
| `--bigmac-price` | | Big Mac price with its currency, e.g. `"7.30 CHF"`; overrides `--country` |
| `--bigmac-url` | The Economist's July 2026 CSV | Big Mac index CSV |
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

`chain.go` holds the transaction building, signing and broadcast; `bigmac.go` the Big Mac price; `market.go` the order book and fair value; `fx.go` the currency conversion; `feed.go` the ratio; `main.go` wires them.
