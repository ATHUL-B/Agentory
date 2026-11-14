# Agentory declarative runtime: runs `runtime: declarative` Agents.
FROM python:3.12.12-slim
WORKDIR /app
COPY runtime.py .
USER 65534:65534
EXPOSE 8080
CMD ["python", "-u", "/app/runtime.py"]
