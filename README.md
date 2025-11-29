# Track Info Fetcher

This project retrieves metadata about tracks hosted on **KrakenFiles** and **Imgur.gg** using the ArtistGrid info service.

## Supported Providers

* **Imgur.gg** via: `https://info.artistgrid.cx/imgur/?id=<ID>`
* **KrakenFiles** via: `https://info.artistgrid.cx/kf/?id=<ID>`

### Example Requests

* Imgur.gg: `https://info.artistgrid.cx/imgur/?id=pDHDW37`
* KrakenFiles: `https://info.artistgrid.cx/kf/?id=TgGjXDjG5M`

## Environment Configuration

Set the required environment variables in your `.env` file:

```env
REDIS_URL=
DEBUG=true
```

### Notes

* `DEBUG=true` disables caching.
* `DEBUG=false` enables Redis caching.
