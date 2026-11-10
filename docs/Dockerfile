# Swagger UI with the Pearlora OpenAPI spec baked in.
# Build from the repo root: docker build -f docs/Dockerfile .
FROM swaggerapi/swagger-ui:v5.17.14
# Default host shown in "Try it out" (editable in the UI).
ARG API_HOST=localhost
COPY openapi.yaml /spec/openapi.yaml
RUN sed -i "s/default: localhost/default: ${API_HOST}/" /spec/openapi.yaml
ENV SWAGGER_JSON=/spec/openapi.yaml
