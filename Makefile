# Benchmarks require the backend to be running: cd backend && go run .

.PHONY: bench bench-contention bench-throughput bench-fanout bench-latency

bench:
	cd load-tester && go test -bench=. -benchtime=1x -v

bench-contention:
	cd load-tester && go test -bench=BenchmarkSingleDocContention -benchtime=1x -v

bench-throughput:
	cd load-tester && go test -bench=BenchmarkThroughputSaturation -benchtime=1x -v

bench-fanout:
	cd load-tester && go test -bench=BenchmarkManyDocFanout -benchtime=1x -v

bench-latency:
	cd load-tester && go test -bench=BenchmarkRoundTripLatency -benchtime=1x -v
