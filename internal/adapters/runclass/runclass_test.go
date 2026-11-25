package runclass

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd  string
		kind string // "" = not a run
	}{
		{"npm test", "test"},
		{"npm run test:unit -- --watch", "test"},
		{"pnpm test", "test"},
		{"yarn test", "test"},
		{"bun test", "test"},
		{"npx jest --coverage", "test"},
		{"npx vitest run", "test"},
		{"pytest -x tests/", "test"},
		{"python -m pytest", "test"},
		{"python3 -m unittest discover", "test"},
		{"uv run pytest", "test"},
		{"poetry run pytest -q", "test"},
		{"cargo test --lib", "test"},
		{"cargo nextest run", "test"},
		{"go test ./...", "test"},
		{"go test -race ./internal/...", "test"},
		{"make test", "test"},
		{"make check", "test"},
		{"./gradlew test", "test"},
		{"gradle :app:testDebugUnitTest", "test"},
		{"mvn -q verify", "test"},
		{"dotnet test", "test"},
		{"bundle exec rspec", "test"},
		{"cd app && npm test", "test"},
		{"CI=1 npm test", "test"},
		{"sudo -E npm test", "test"},
		{"bash -c 'cd x && cargo test'", "test"},
		{"cmd /c npm test", "test"},
		{`"C:\Program Files\nodejs\npm.cmd" test`, "test"},
		{"deno test", "test"},
		{"swift test", "test"},
		{"flutter test", "test"},
		{"mix test", "test"},
		{"python manage.py test", "test"},

		{"npm run build", "build"},
		{"yarn build", "build"},
		{"pnpm run build:prod", "build"},
		{"tsc --noEmit", "build"},
		{"vite build", "build"},
		{"next build", "build"},
		{"cargo build --release", "build"},
		{"cargo check", "build"},
		{"go build ./cmd/seatime", "build"},
		{"go build -o out . 2>&1 | tail", "build"},
		{"make", "build"},
		{"make -j4 all", "build"},
		{"gradle build", "build"},
		{"./gradlew assembleDebug", "build"},
		{"mvn package", "build"},
		{"dotnet build", "build"},
		{"docker build -t x .", "build"},
		{"gcc -o main main.c", "build"},
		{"cmake --build build", "build"},
		{"cd x && make && ./main", "build"},

		{"npm run dev", "dev"},
		{"npm start", "dev"},
		{"pnpm dev", "dev"},
		{"yarn start", "dev"},
		{"vite", "dev"},
		{"next dev", "dev"},
		{"flask run --debug", "dev"},
		{"uvicorn app:app --reload", "dev"},
		{"python manage.py runserver", "dev"},
		{"rails server", "dev"},
		{"docker compose up -d", "dev"},
		{"docker-compose up", "dev"},
		{"python -m http.server 8000", "dev"},
		{"cargo watch -x run", "dev"},
		{"nodemon server.js", "dev"},

		{"python main.py", "exec"},
		{"python3 -u script.py --flag", "exec"},
		{"uv run main.py", "exec"},
		{"node index.js", "exec"},
		{"npx tsx src/a.ts", "exec"},
		{"bun run index.ts", "exec"},
		{"go run .", "exec"},
		{"cargo run -- --help", "exec"},
		{"ruby app.rb", "exec"},
		{"java -jar target/app.jar", "exec"},
		{"./a.out", "exec"},
		{"./run.sh", "exec"},
		{"dotnet run", "exec"},
		{"python -m mypkg", "exec"},

		{"git status", ""},
		{"ls -la", ""},
		{"cat README.md", ""},
		{"grep -rn foo .", ""},
		{"npm install", ""},
		{"pip install -r requirements.txt", ""},
		{"python -c 'print(1)'", ""},
		{"python --version", ""},
		{"node -e 'console.log(1)'", ""},
		{"python -m pip install x", ""},
		{"echo npm test", ""},
		{"cargo fmt", ""},
		{"go mod tidy", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, ok := Classify(c.cmd)
		if c.kind == "" {
			if ok {
				t.Errorf("Classify(%q) = %q, want none", c.cmd, got)
			}
			continue
		}
		if !ok || got != c.kind {
			t.Errorf("Classify(%q) = %q,%v want %q", c.cmd, got, ok, c.kind)
		}
	}
}
