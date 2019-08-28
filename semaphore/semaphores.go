package semaphore

type Semaphore struct {
	channel chan int8
}

func (s *Semaphore) TryAcquire() bool {
	select {
	case s.channel <- int8(1):
		return true
	default:
		return false
	}
}

func (s *Semaphore) Acquire() {
	s.channel <- int8(1)
}

func (s *Semaphore) Release() {
	<-s.channel
}

func NewSemaphore(concurrency int) *Semaphore {
	return &Semaphore{make(chan int8, concurrency)}
}
