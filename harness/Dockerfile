# Static stub for the FE domain: nginx serves the harness files as-is.
# No build step.
FROM nginx:alpine
COPY index.html app.js fixtures.json /usr/share/nginx/html/
EXPOSE 80
