# syntax=docker/dockerfile:1

# Same Node as the Makefile (NODE) and the root Dockerfile (NODE_VERSION).
# @mega-yfue/eufy-sdk requires Node >= 24.5.
FROM node:24.22.1-slim

# ffmpeg decodes a short live burst when Eufy has no retained push thumbnail.
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --omit=dev --no-audit --no-fund
COPY src ./src
RUN mkdir -p /data && chown -R node:node /app /data

ENV EUFY_SESSION_DIR=/data
USER node
EXPOSE 3000
CMD ["node", "src/main.ts"]
