#!/bin/bash

# --- Color Definitions ---
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${BLUE}======================================================"
echo -e "      API GATEWAY AUTOMATION & TEST ORCHESTRATOR      "
echo -e "======================================================${NC}"

# 1. Start Docker Containers
echo -e "\n${GREEN}🚀 Starting Docker services...${NC}"
./.bin/docker-compose up --build -d

# 2. Wait for API Gateway readiness
echo -e "\n${YELLOW}⏳ Waiting for API Gateway to be ready on port 8080...${NC}"
until curl -s http://localhost:8080/metrics > /dev/null; do
    printf "."
    sleep 1
done
echo -e "\n${GREEN}✅ API Gateway is fully operational!${NC}"

# 3. Generate Auth Tokens
echo -e "\n${GREEN}🔑 Generating JWT tokens using Go tool...${NC}"
if command -v go >/dev/null 2>&1; then
    TOKENS_OUT=$(go run generatetoken.go)
elif [ -f "./gen-token" ]; then
    TOKENS_OUT=$(./gen-token)
else
    echo -e "${RED}❌ Error: Neither 'go' command nor './gen-token' executable found!${NC}"
    exit 1
fi

PREMIUM_TOKEN=$(echo "$TOKENS_OUT" | grep -A 1 "PREMIUM TOKEN:" | tail -n 1 | awk '{print $3}')
FREE_TOKEN=$(echo "$TOKENS_OUT" | grep -A 1 "FREE TOKEN:" | tail -n 1 | awk '{print $3}')

if [ -z "$PREMIUM_TOKEN" ] || [ -z "$FREE_TOKEN" ]; then
    echo -e "${RED}❌ Error: Failed to parse tokens from generation tool output.${NC}"
    exit 1
fi

echo -e "${GREEN}Tokens generated successfully:${NC}"
echo -e "  - Premium Token (first 25 chars): ${BLUE}${PREMIUM_TOKEN:0:25}...${NC}"
echo -e "  - Free Token (first 25 chars):    ${BLUE}${FREE_TOKEN:0:25}...${NC}"

# 4. Start Traffic Simulation Loop in background
echo -e "\n${GREEN}📈 Initiating background Traffic Simulator...${NC}"

traffic_generator() {
  count=0
  while true; do
    # A. Premium steady traffic to products-service (every 0.8s)
    # This will show load balancing round-robin between Products-1 and Products-2
    curl -s -H "Authorization: Bearer $PREMIUM_TOKEN" http://localhost:8080/api/v1/products >/dev/null &
    
    # B. Premium occasional traffic to checkout-service (every 2.4s)
    if [ $((count % 3)) -eq 0 ]; then
      curl -s -H "Authorization: Bearer $PREMIUM_TOKEN" http://localhost:8080/api/v1/checkout >/dev/null &
    fi

    # C. Free user steady traffic (every 3.2s) - well within free limits
    if [ $((count % 4)) -eq 0 ]; then
      curl -s -H "Authorization: Bearer $FREE_TOKEN" http://localhost:8080/api/v1/products >/dev/null &
    fi

    # D. Free user burst traffic (every 16s - 8 requests in a burst)
    # This will deplete the free tier bucket (capacity 5) and trigger 429 Too Many Requests
    if [ $((count % 20)) -eq 0 ] && [ $count -ne 0 ]; then
      echo -e "\n${YELLOW}💥 Triggering Free User Rate Limit Burst (8 parallel requests)...${NC}"
      for i in {1..8}; do
        curl -s -H "Authorization: Bearer $FREE_TOKEN" http://localhost:8080/api/v1/products >/dev/null &
      done
    fi

    # E. Health checker & failover simulation (every 40s toggle)
    # Stops and starts service-products-2 to demonstrate live recovery & status update
    if [ $((count % 50)) -eq 0 ] && [ $count -ne 0 ]; then
      if [ $(( (count / 50) % 2 )) -eq 1 ]; then
        echo -e "\n${RED}⚠️ Simulating Service Products-2 Crash: Stopping container...${NC}"
        ./.bin/docker-compose stop service-products-2 >/dev/null 2>&1 &
      else
        echo -e "\n${GREEN}🔄 Simulating Service Products-2 Recovery: Starting container...${NC}"
        ./.bin/docker-compose start service-products-2 >/dev/null 2>&1 &
      fi
    fi

    count=$((count + 1))
    sleep 0.8
  done
}

# Run traffic generator in the background
traffic_generator &
TRAFFIC_PID=$!

cleanup() {
    echo -e "\n${YELLOW}🛑 Shutting down traffic simulator (PID: $TRAFFIC_PID)...${NC}"
    kill $TRAFFIC_PID 2>/dev/null
    echo -e "${YELLOW}Do you want to stop the Docker containers too? (y/N)${NC}"
    read -r -t 5 response
    if [[ "$response" =~ ^([yY][eE][sS]|[yY])$ ]]; then
        echo -e "${RED}Stopping Docker services...${NC}"
        ./.bin/docker-compose down
    else
        echo -e "${GREEN}Leaving Docker containers running. Run './.bin/docker-compose down' to clean up later.${NC}"
    fi
    exit 0
}

trap cleanup SIGINT SIGTERM

echo -e "\n${GREEN}======================================================"
echo -e "🎉 Setup Complete! Dashboard is now running."
echo -e "👉 Open: ${BLUE}http://localhost:3001${NC} in your browser."
echo -e "======================================================${NC}"
echo -e "Simulation running. Press [Ctrl+C] to stop simulation and clean up."

# Keep script running to maintain traffic generator and trap
wait $TRAFFIC_PID
