# foldbase — the append-only event log + read models over HTTP.
#
# Every dependency is a published npm package (@baseworks/{eventstore,config,
# readmodel}, @getflect/sdk, hono, …), so this builds STANDALONE — the build
# context is this directory, not the monorepo root:
#   docker build -t ghcr.io/dotlabshq/foldbase .
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json ./
RUN npm install
COPY tsconfig.json tsup.config.ts ./
COPY src ./src
RUN npm run build

FROM node:22-alpine
WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev
COPY --from=build /app/dist ./dist

ENV NODE_ENV=production
ENV PORT=3001
EXPOSE 3001

CMD ["node", "dist/index.js"]
