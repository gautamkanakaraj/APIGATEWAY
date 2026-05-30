#!/bin/bash
TOKEN="eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3ODAxNDgzOTYsInN1YiI6InVzZXJfcHJlbWl1bV85OSIsInRpZXIiOiJwcmVtaXVtIn0.lp_FlBD3PpoqbxLJMm-vMyjuDZW0X_-4ooWqQ-JQao8"

echo "🚀 Starting Premium Tier Loop Test (10 Requests)..."
for i in {1..10}; do 
  echo -n "Premium Request #$i -> Status Code: "
  curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/products
done
