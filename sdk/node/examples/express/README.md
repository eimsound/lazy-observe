# Express example

```sh
mise exec -- pnpm install
mise exec -- pnpm build
LO_DSN=http://127.0.0.1:3100/p/default@TOKEN mise exec -- pnpm --filter lazy-observe-express-example start
curl http://127.0.0.1:3200/
curl http://127.0.0.1:3200/error
```
