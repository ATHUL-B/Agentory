# Reference "bring your own container" A2A agent for Agentory.
FROM python:3.12.12-slim
WORKDIR /app
COPY agent.py .
USER 65534:65534
EXPOSE 8080
CMD ["python", "-u", "/app/agent.py"]
