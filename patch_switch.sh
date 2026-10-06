sed -i '' 's|"net/http"|"net/http"\n\t"fmt"\n\n\t"github.com/google/uuid"|' internal/server/handlers_run_control.go
