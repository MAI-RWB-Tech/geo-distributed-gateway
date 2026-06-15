FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

ENV PYTHONPATH=/app

EXPOSE 8000

HEALTHCHECK --interval=10s --timeout=5s --retries=5 --start-period=30s \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:8000/health')" || exit 1

# Generate a stub model if no real model is present, then start the server.
# The stub is replaced automatically when a volume-mounted trained model exists.
ENTRYPOINT ["sh", "-c", "python /app/scripts/generate_stub_model.py /app/models/latest && exec uvicorn src.api.main:app --host 0.0.0.0 --port 8000"]
