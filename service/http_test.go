package service

import (
	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Service HTTP", func() {
	var err error

	Describe("HTTPInfo", func() {
		It("returns the expected router", func() {
			endpoints := EndpointsFromAddrs("proto", []string{":1", "localhost:2"})
			sut := HTTPInfo{Info{"name", endpoints}, chi.NewMux()}

			Expect(sut.ServiceName()).Should(Equal("name"))
			Expect(sut.ExposeOn()).Should(Equal(endpoints))
		})
	})

	Describe("httpMerger", func() {
		httpSvcA1 := newFakeHTTPService("HTTP A1", "a:1")
		httpSvcA1_ := newFakeHTTPService("HTTP A1_", "a:1")
		httpSvcB1 := newFakeHTTPService("HTTP B1", "b:1")

		sut := newHTTPMerger(httpSvcA1)

		nonHTTPSvc := &Info{"non HTTP service", EndpointsFromAddrs("proto", []string{":1"})}

		It("uses the given service", func() {
			Expect(sut.String()).Should(Equal(httpSvcA1.String()))
			Expect(sut.Router()).Should(BeIdenticalTo(httpSvcA1.Router()))
			Expect(sut.ExposeOn()).Should(Equal(httpSvcA1.ExposeOn()))
		})

		It("can merge other HTTP services", func() {
			merged, err := sut.Merge(httpSvcA1_)
			Expect(err).Should(Succeed())
			Expect(merged).Should(BeIdenticalTo(sut))
			Expect(merged.String()).Should(SatisfyAll(
				ContainSubstring(httpSvcA1.ServiceName()),
				ContainSubstring(httpSvcA1_.ServiceName())),
			)

			By("merging the common endpoints", func() {
				Expect(merged.ExposeOn()).Should(Equal(httpSvcA1.ExposeOn()))
			})

			By("merging another service again", func() {
				merged, err = sut.Merge(httpSvcB1)
				Expect(err).Should(Succeed())
				Expect(merged).Should(BeIdenticalTo(sut))
			})

			By("excluding non-common endpoints", func() {
				Expect(merged.ExposeOn()).Should(BeEmpty())
			})

			By("including all HTTP routes", func() {
				Expect(sut.Router().Routes()).Should(HaveLen(3))
			})
		})

		It("cannot merge a non HTTP service", func() {
			_, err = sut.Merge(nonHTTPSvc)
			Expect(err).Should(MatchError(ContainSubstring("not an HTTPService")))
		})
	})
})

type fakeHTTPService struct {
	HTTPInfo
}

func newFakeHTTPService(name string, addrs ...string) *fakeHTTPService {
	mux := chi.NewMux()
	mux.Get("/"+name, nil)

	return &fakeHTTPService{HTTPInfo{
		Info: Info{Name: name, Endpoints: EndpointsFromAddrs("http", addrs)},
		Mux:  mux,
	}}
}

func (s *fakeHTTPService) Merge(other Service) (Merger, error) {
	return MergeHTTP(s, other)
}
