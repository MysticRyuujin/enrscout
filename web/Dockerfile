# syntax=docker/dockerfile:1.7
FROM node:24-alpine@sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts --no-audit --no-fund
COPY web/ .
ARG VITE_API_BASE=
ARG VITE_NETWORKS=
ENV VITE_API_BASE=$VITE_API_BASE VITE_NETWORKS=$VITE_NETWORKS
RUN npm run build

FROM nginxinc/nginx-unprivileged:alpine@sha256:a718212f9cf21e241f14067333000a3f0930292f5354fe0db269e9a2a2596b9e
COPY --from=build /web/dist /usr/share/nginx/html
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY LICENSE NOTICE /licenses/enrscout/
LABEL org.opencontainers.image.source="https://github.com/MysticRyuujin/enrscout" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
EXPOSE 8081
