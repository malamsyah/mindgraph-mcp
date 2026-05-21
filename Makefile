REGION     ?= asia-northeast1
SERVICE    ?= mindgraph
REPO       ?= mindgraph
PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null)
IMAGE      ?= $(REGION)-docker.pkg.dev/$(PROJECT_ID)/$(REPO)/$(SERVICE):latest

SECRETS = MINDGRAPH_API_KEY=mindgraph-api-key:latest,\
NEO4J_URI=neo4j-uri:latest,\
NEO4J_USER=neo4j-user:latest,\
NEO4J_PASSWORD=neo4j-password:latest,\
VOYAGE_API_KEY=voyage-api-key:latest

.PHONY: help build deploy release logs url

help:
	@echo "Targets:"
	@echo "  build    - submit image to Cloud Build → Artifact Registry"
	@echo "  deploy   - deploy the latest image to Cloud Run"
	@echo "  release  - build then deploy"
	@echo "  logs     - tail recent Cloud Run logs"
	@echo "  url      - print the Cloud Run service URL"
	@echo ""
	@echo "Overrides: REGION=$(REGION) SERVICE=$(SERVICE) PROJECT_ID=$(PROJECT_ID)"

build:
	@test -n "$(PROJECT_ID)" || (echo "PROJECT_ID is empty; run: gcloud config set project YOUR-ID" && exit 1)
	gcloud builds submit --tag $(IMAGE)

deploy:
	@test -n "$(PROJECT_ID)" || (echo "PROJECT_ID is empty; run: gcloud config set project YOUR-ID" && exit 1)
	gcloud run deploy $(SERVICE) \
	  --image $(IMAGE) \
	  --region $(REGION) \
	  --platform managed \
	  --allow-unauthenticated \
	  --min-instances 1 \
	  --max-instances 4 \
	  --cpu 2 \
	  --memory 1Gi \
	  --timeout 60s \
	  --concurrency 80 \
	  --set-secrets="$(SECRETS)"

release: build deploy

logs:
	gcloud run services logs read $(SERVICE) --region $(REGION) --limit 100

url:
	@gcloud run services describe $(SERVICE) --region $(REGION) --format='value(status.url)'
