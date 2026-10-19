# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:6f08a523443ddedf52941874c203475b0ad503e859ad70cb26ab8be9a517dbd5 AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts --no-audit --no-fund
COPY web/ .
ARG VITE_API_BASE=
ARG VITE_NETWORKS=
ENV VITE_API_BASE=$VITE_API_BASE VITE_NETWORKS=$VITE_NETWORKS
RUN npm run build

FROM nginxinc/nginx-unprivileged:alpine@sha256:b5047b9b76845eac124358373c6852377c52e4b05834a47647e955dd91cb8f1e
COPY --from=build /web/dist /usr/share/nginx/html
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY LICENSE NOTICE /licenses/enrscout/
LABEL org.opencontainers.image.source="https://github.com/MysticRyuujin/enrscout" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
EXPOSE 8081
