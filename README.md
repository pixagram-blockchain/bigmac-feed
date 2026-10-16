# bigmac-feed

Price feed publisher for [Pixagram](https://pixagram.com) witnesses.

The feed values 1 PXS at the price of one US Big Mac. Every hour it:

1. reads the latest US Big Mac price from The Economist's [Big Mac index](https://www.economist.com/big-mac-index);
2. reads the PIXA price in USD from Coinstore (`PIXAUSDT`, last trade);
3. publishes `1.000 PXS = <Big Mac price / PIXA price> PIXA` for each witness.

## Run

```bash
docker run -d --name bigmac-feed --restart unless-stopped \
  -e WITNESS_WIF=<active key> \
  pixadock/bigmac-feed:v1.1.0 \
  --witness=<witness name>
```

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--witness` | `initminer` | Witness account(s), comma-separated |
| `--wif` | `WITNESS_WIF` env | Active key(s), in the same order as `--witness` |
| `--rpc` | `https://api.pixagram.com` | Node to broadcast through |
| `--chain-id` | read from `--rpc` | Chain ID in hex |
| `--token-price` | `0.12` | PIXA price in USD, used until `PIXAUSDT` has trades |
| `--interval` | `1h` | How often to publish |
| `--once` | `false` | Publish once and exit |

## Build

```bash
docker build -t bigmac-feed .
```
