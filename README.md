# bigmac-feed

Price feed publisher for [Pixagram](https://pixagram.com) witnesses.

The feed values 1 PXS at the price of one Big Mac in the country the feeder runs in. On startup it picks the country:

1. `--country`, if set;
2. otherwise the country of the server's public IP, looked up once via [ipinfo.io](https://ipinfo.io);
3. otherwise, if that lookup fails, the United States.

Then every hour it:

1. reads that country's Big Mac price in USD from the latest survey of The Economist's [Big Mac index](https://www.economist.com/big-mac-index). Euro area countries use the `Euro area` price. A detected country that is not in the latest survey uses the US price; a `--country` that is not in it fails the round;
2. reads the PIXA price in USD from Coinstore (`PIXAUSDT`, last trade);
3. publishes `1.000 PXS = <Big Mac price / PIXA price> PIXA` for each witness.

Every network request times out after 60 s. A failed round is logged and retried at the next interval.

## Run

```bash
docker run -d --name bigmac-feed --restart unless-stopped \
  -e WITNESS_WIF=<active key> \
  pixadock/bigmac-feed:v1.2.0 \
  --witness=<witness name>
```

Add `--country=CH` (or any other country code) to use a fixed country instead of the detected one.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--witness` | `initminer` | Witness account(s), comma-separated |
| `--wif` | `WITNESS_WIF` env | Active key(s), in the same order as `--witness` |
| `--rpc` | `https://api.pixagram.com` | Node to broadcast through |
| `--chain-id` | read from `--rpc` | Chain ID in hex |
| `--country` | detected from IP | Country whose Big Mac price to use: ISO 3166 code (`CN`, `CHN`), or `EUZ` for the euro area |
| `--token-price` | `0.12` | PIXA price in USD, used until `PIXAUSDT` has trades |
| `--interval` | `1h` | How often to publish |
| `--once` | `false` | Publish once and exit |

## Build

```bash
docker build -t bigmac-feed .
```
