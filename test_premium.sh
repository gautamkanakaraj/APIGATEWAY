#!/bin/bash
TOKEN="eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3ODIzMTk4OTcsInN1YiI6InVzZXJfZnJlZV8xMSIsInRpZXIiOiJmcmVlIn0.f9dOXL1EJjFVRdjCU8y7ZS7eox9fEKXL3Cgg9EkDXeU"

echo "Starting Premium Tier Loop Test (10 Requests)..."
for i in {1..10}; do 
  echo -n "Premium Request #$i -> Status Code: "
  curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/products
done
