package main

import (
	"errors"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"os"
	"wekactl/internal/aws/lambdas"
	"wekactl/internal/env"
)


func joinHandler() (events.APIGatewayProxyResponse, error) {
	result, err := lambdas.GetJoinParams(
		os.Getenv("ASG_NAME"),
		os.Getenv("TABLE_NAME"),
	)
	if err != nil {
		result = err.Error()
	}
	return events.APIGatewayProxyResponse{Body: result, StatusCode: 200}, nil
}


func fetchHandler() (lambdas.FetchData, error) {
	result, err := lambdas.GetFetchDataParams(
		os.Getenv("ASG_NAME"),
		os.Getenv("TABLE_NAME"),
	)
	if err != nil {
		return lambdas.FetchData{}, err
	}
	return result, nil
}


func main() {
	env.Config.Region = os.Getenv("REGION")
	switch lambdaType := os.Getenv("LAMBDA"); lambdaType {
	case "join":
		lambda.Start(joinHandler)
	case "fetch":
		lambda.Start(fetchHandler)
	default:
		lambda.Start(func() error {return errors.New("unsupported lambda command")})
	}
}