# Builds the React app and serves it with nginx.
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.29-alpine
# The nginx image turns templates into config at start, filling ${...} from the environment.
ENV PORT=80 API_UPSTREAM=http://api:8080 API_HOST=api DNS_RESOLVER=127.0.0.11
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
