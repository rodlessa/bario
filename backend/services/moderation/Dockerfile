# syntax=docker/dockerfile:1
FROM node:22-bookworm-slim AS deps

WORKDIR /app

# NSFWJS 4.x ships the MobileNetV2 weights inside the npm package, so the
# image needs no model download at runtime. tfjs-node fetches its native
# libtensorflow dependency during npm ci (build-time network only).
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund \
    && npm cache clean --force

FROM node:22-bookworm-slim

ENV NODE_ENV=production \
    HTTP_PORT=8085 \
    METRICS_PORT=9095

WORKDIR /app

COPY --from=deps --chown=node:node /app/node_modules ./node_modules
COPY --chown=node:node package.json ./
COPY --chown=node:node src ./src

USER node

EXPOSE 8085 9095

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD node -e "fetch('http://127.0.0.1:' + (process.env.HTTP_PORT || 8085) + '/healthz').then((r) => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"

CMD ["node", "src/server.js"]
